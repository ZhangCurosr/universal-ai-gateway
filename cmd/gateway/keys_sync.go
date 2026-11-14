package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// gateway keys sync（CLI-13 ~ CLI-16，任务卡 T64）：让池中带同一标签的 Key 与账号文件保持一致——
// 新增、更新元数据、停用文件中已经没有（或不再符合条件）的 Key、恢复由同步停用后又回到文件中的 Key。
// 与 keys import 一样，任何输出都不包含密钥或文件中的其他字段。

// syncDisabledReason 是同步停用 Key 时写入的原因；只有这个原因的 Key 会被同步恢复（CLI-13）。
const syncDisabledReason = "sync: not in source"

// maxKeyListBytes 是读取 Key 列表的响应上限（一把 Key 的视图约 0.5 KiB）。
const maxKeyListBytes = 64 << 20

type syncKey struct {
	ID          int64  `json:"id"`
	Pool        string `json:"pool"`
	Name        string `json:"name"`
	State       string `json:"state"`
	StoredState string `json:"stored_state"`
	StateReason string `json:"state_reason"`
	Label       string `json:"label"`
}

// persisted 返回库中的状态（展示状态 expired、exhausted、orphaned 时取 stored_state）。
func (k syncKey) persisted() string {
	if k.StoredState != "" {
		return k.StoredState
	}
	return k.State
}

func runSync(ctx context.Context, o *importOpts, stdout, stderr io.Writer) int {
	if o.watch == 0 {
		line, code := syncOnce(ctx, o, stderr)
		fmt.Fprintln(stdout, line)
		return code
	}
	// CLI-15：文件修改时间或大小变化才同步；单次失败只报告，继续运行
	var lastMod time.Time
	var lastSize int64 = -1
	t := time.NewTicker(o.watch)
	defer t.Stop()
	for {
		if fi, err := os.Stat(o.file); err != nil {
			fmt.Fprintf(stderr, "%s gateway: %v\n", time.Now().Format(time.DateTime), err)
		} else if !fi.ModTime().Equal(lastMod) || fi.Size() != lastSize {
			line, code := syncOnce(ctx, o, stderr)
			fmt.Fprintf(stdout, "%s %s\n", time.Now().Format(time.DateTime), line)
			if code == 0 { // 失败时下一轮重试
				lastMod, lastSize = fi.ModTime(), fi.Size()
			}
		}
		select {
		case <-ctx.Done():
			return 0
		case <-t.C:
		}
	}
}

// syncOnce 执行一次同步，返回汇总行（CLI-16）与退出码。
func syncOnce(ctx context.Context, o *importOpts, stderr io.Writer) (string, int) {
	inSource := map[int64]bool{}
	st, code := runImport(ctx, o, stderr, func(id int64) {
		if id > 0 {
			inSource[id] = true
		}
	})
	if st == nil {
		return "sync failed (see the error above)", code // 读文件或读取 Key 列表失败
	}
	if o.dryRun {
		return fmt.Sprintf("would sync: %d keys in the source, skipped %d, expired %d (disable and restore are decided against the gateway, not in --dry-run)",
			st.imported, st.skipped, st.expired), 0
	}
	summary := func(disabled, restored int) string {
		return fmt.Sprintf("synced: imported %d, updated %d, disabled %d, restored %d, skipped %d, expired %d, failed %d",
			st.imported, st.updated+st.duplicate, disabled, restored, st.skipped, st.expired, st.failed)
	}
	if code != 0 { // 有行失败：无法确定它们是否在源中，这一轮不停用任何 Key
		fmt.Fprintln(stderr, "gateway: some rows failed; no keys were disabled or restored in this run")
		return summary(0, 0), code
	}
	keys, err := listSyncKeys(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "gateway: list keys:", redactToken(err.Error(), o.token))
		return summary(0, 0), 1
	}
	var disable, restore []int64
	labeled := 0
	for _, k := range keys {
		if k.Pool != o.pool || k.Label != o.label {
			continue
		}
		labeled++
		switch p := k.persisted(); {
		case p == "ready" && !inSource[k.ID]:
			disable = append(disable, k.ID)
		case p == "disabled" && k.StateReason == syncDisabledReason && inSource[k.ID]:
			restore = append(restore, k.ID)
		}
	}
	code = 0
	if len(disable) >= 2 && len(disable)*2 > labeled && !o.force { // CLI-14
		fmt.Fprintf(stderr, "gateway: refusing to disable %d of %d keys labeled %q (more than half); check the file, or use --force\n",
			len(disable), labeled, o.label)
		disable, code = nil, 1
	}
	client := newCLIHTTPClient()
	patch := func(id int64, body string) bool {
		status, resp, err := adminRequest(ctx, client, http.MethodPatch, o.base+"/admin/api/keys/"+strconv.FormatInt(id, 10), o.token, []byte(body))
		if err != nil || status != http.StatusOK {
			why := ""
			if err != nil {
				why = redactToken(err.Error(), o.token)
			} else {
				why = fmt.Sprintf("HTTP %d: %s", status, apiErrorMessage(resp))
			}
			fmt.Fprintf(stderr, "key %d: failed (%s)\n", id, why)
			return false
		}
		return true
	}
	disabled, restored := 0, 0
	for _, id := range disable {
		if patch(id, `{"state":"disabled","reason":"`+syncDisabledReason+`"}`) {
			disabled++
		} else {
			code = 1
		}
	}
	for _, id := range restore {
		if patch(id, `{"state":"ready"}`) {
			restored++
		} else {
			code = 1
		}
	}
	return summary(disabled, restored), code
}

// listSyncKeys 读取全部 Key 的视图（不含密钥，ADM-16）。
func listSyncKeys(ctx context.Context, o *importOpts) ([]syncKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/admin/api/keys", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	resp, err := newCLIHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // 只读响应体，关闭失败无可处理
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyListBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxKeyListBytes {
		return nil, fmt.Errorf("key list is larger than %d MiB", maxKeyListBytes>>20)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, apiErrorMessage(raw))
	}
	var out struct {
		Keys []syncKey `json:"keys"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode key list: %w", err)
	}
	return out.Keys, nil
}
