package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func (s *server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req CreateKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	// ADM-13: pool 必须在配置中存在（否则 400）
	poolFound := false
	if cfg := s.config(); cfg != nil {
		for _, p := range cfg.Pools {
			if p.Name == req.Pool {
				poolFound = true
				break
			}
		}
	}
	if !poolFound {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("pool %q is not configured", req.Pool)))
		return
	}

	name := strings.TrimSpace(req.Name)
	if len(name) == 0 || len(req.Name) > 64 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "name must be between 1 and 64 characters"))
		return
	}

	// ADM-13: secret 非空、<= 4096 字节
	if len(req.Secret) == 0 || len(req.Secret) > 4096 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "secret must be non-empty and at most 4096 bytes"))
		return
	}

	weight := 1
	if req.Weight != nil {
		if *req.Weight <= 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "weight must be > 0"))
			return
		}
		weight = *req.Weight
	}

	maxInFlight := 16
	if req.MaxInFlight != nil {
		if *req.MaxInFlight <= 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "max_in_flight must be > 0"))
			return
		}
		maxInFlight = *req.MaxInFlight
	}

	var expiresAt time.Time
	if req.ExpiresAt != "" {
		t, err := parseExpiresAt(req.ExpiresAt)
		if err != nil {
			writeError(w, err)
			return
		}
		expiresAt = t
	}
	if err := validLabel(req.Label); err != nil {
		writeError(w, err)
		return
	}

	if s.d.Keyring == nil {
		writeError(w, apperr.New(apperr.CodeInternal, "keyring not available"))
		return
	}

	// ADM-13: fingerprint = HashToken(pepper, secret)
	fingerprint := auth.HashToken(s.d.Keyring.TokenPepper(), req.Secret)
	secretHint := obs.MaskSecret(req.Secret)

	seal := func(id int64) (crypto.Sealed, error) {
		aad := crypto.UpstreamKeyAAD(id, req.Pool, s.d.Keyring.Version())
		return s.d.Keyring.Seal([]byte(req.Secret), aad)
	}

	k := &store.UpstreamKey{
		Pool:        req.Pool,
		Name:        name,
		Fingerprint: fingerprint,
		SecretHint:  secretHint,
		Weight:      weight,
		MaxInFlight: maxInFlight,
		State:       store.KeyStateReady,
		ExpiresAt:   expiresAt,
		Label:       req.Label,
	}

	id, err := s.d.Store.InsertUpstreamKey(r.Context(), k, seal)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.writeKeyConflict(w, r, fingerprint)
			return
		}
		writeError(w, err)
		return
	}
	k.ID = id

	// ADM-14: 创建 Key 后，同步执行 scheduler.LoadKeys + Scheduler.SetKeys
	if s.d.Scheduler != nil {
		loadedKeys, err := scheduler.LoadKeys(r.Context(), s.d.Store, s.d.Keyring, s.d.Logger)
		if err != nil {
			s.d.Logger.Error("admin: reload keys failed after create", "key_id", id, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "failed to reload scheduler keys", err))
			return
		}
		s.d.Scheduler.SetKeys(loadedKeys)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"key": s.buildKeyView(k),
	})
}

func (s *server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.d.Store.ListUpstreamKeys(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}

	stMap := s.keyStatusMap()
	pools := s.configuredPools()
	views := make([]KeyView, 0, len(keys))
	for i := range keys {
		st, ok := stMap[keys[i].ID]
		var stPtr *scheduler.KeyStatus
		if ok {
			stPtr = &st
		}
		views = append(views, markOrphaned(buildKeyViewWithStatus(&keys[i], stPtr), pools))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"keys": views,
	})
}

func (s *server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	_, err = s.d.Store.GetUpstreamKey(r.Context(), id) // 先确认存在，不存在直接 404，避免部分写入
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	var req UpdateKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if len(trimmed) == 0 || len(*req.Name) > 64 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "name must be between 1 and 64 characters"))
			return
		}
	}

	if req.Weight != nil && *req.Weight <= 0 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "weight must be > 0"))
		return
	}

	if req.MaxInFlight != nil && *req.MaxInFlight <= 0 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "max_in_flight must be > 0"))
		return
	}

	// ADM-15: PATCH keys 的 state 只接受 ready / disabled；设为 ready 会清除 quarantined 状态与 state_reason
	if req.State != nil {
		if *req.State != store.KeyStateReady && *req.State != store.KeyStateDisabled {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "state must be ready or disabled"))
			return
		}
	}
	// ADM-27：停用原因
	if req.Reason != nil {
		if req.State == nil || *req.State != store.KeyStateDisabled {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "reason is only allowed together with state disabled"))
			return
		}
		if r := strings.TrimSpace(*req.Reason); r == "" || len(*req.Reason) > maxReasonLen {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("reason must be between 1 and %d bytes", maxReasonLen)))
			return
		}
	}

	var expiresAt *time.Time
	if len(req.ExpiresAt) > 0 {
		var raw *string
		if err := json.Unmarshal(req.ExpiresAt, &raw); err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "expires_at must be an RFC 3339 string or null"))
			return
		}
		t := time.Time{} // null 或空字符串：清除过期时间
		if raw != nil && *raw != "" {
			parsed, err := parseExpiresAt(*raw)
			if err != nil {
				writeError(w, err)
				return
			}
			t = parsed
		}
		expiresAt = &t
	}
	if req.Label != nil {
		if err := validLabel(*req.Label); err != nil {
			writeError(w, err)
			return
		}
	}

	if req.Name != nil || req.Weight != nil || req.MaxInFlight != nil || expiresAt != nil || req.Label != nil {
		err := s.d.Store.UpdateUpstreamKeyMeta(r.Context(), id, store.UpstreamKeyUpdate{
			Name:        req.Name,
			Weight:      req.Weight,
			MaxInFlight: req.MaxInFlight,
			ExpiresAt:   expiresAt,
			Label:       req.Label,
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
				return
			}
			writeError(w, err)
			return
		}
	}

	if req.State != nil {
		state := *req.State
		reason := ""
		if state == store.KeyStateDisabled {
			reason = "disabled by admin"
			if req.Reason != nil {
				reason = strings.TrimSpace(*req.Reason)
			}
		}
		// state 为 ready 时，reason 为 ""，清除 quarantined 状态与 state_reason (ADM-15)
		err := s.d.Store.SetUpstreamKeyState(r.Context(), id, state, reason)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
				return
			}
			writeError(w, err)
			return
		}
	}

	// ADM-14: 修改 Key 后，同步执行 scheduler.LoadKeys + Scheduler.SetKeys
	if s.d.Scheduler != nil && s.d.Keyring != nil {
		loadedKeys, err := scheduler.LoadKeys(r.Context(), s.d.Store, s.d.Keyring, s.d.Logger)
		if err != nil {
			s.d.Logger.Error("admin: reload keys failed after patch", "key_id", id, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "failed to reload scheduler keys", err))
			return
		}
		s.d.Scheduler.SetKeys(loadedKeys)
	}

	updated, err := s.d.Store.GetUpstreamKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"key": s.buildKeyView(updated),
	})
}

func (s *server) keyStatusMap() map[int64]scheduler.KeyStatus {
	m := make(map[int64]scheduler.KeyStatus)
	if s.d.Scheduler != nil {
		for _, st := range s.d.Scheduler.KeyStatus() {
			m[st.ID] = st
		}
	}
	return m
}

func (s *server) buildKeyView(k *store.UpstreamKey) KeyView {
	stMap := s.keyStatusMap()
	st, ok := stMap[k.ID]
	var stPtr *scheduler.KeyStatus
	if ok {
		stPtr = &st
	}
	return markOrphaned(buildKeyViewWithStatus(k, stPtr), s.configuredPools())
}

// KeyStateOrphaned 是池不在当前配置中的 Key 的展示状态（spec/03 HR-19）；不写库。
const KeyStateOrphaned = "orphaned"

// configuredPools 返回当前配置中的池名集合；没有配置时返回 nil（不判定孤立）。
func (s *server) configuredPools() map[string]bool {
	cfg := s.config()
	if cfg == nil {
		return nil
	}
	m := make(map[string]bool, len(cfg.Pools))
	for _, p := range cfg.Pools {
		m[p.Name] = true
	}
	return m
}

// markOrphaned 实现 HR-19：池不在配置中时 state 显示为 orphaned，库中的状态放进 stored_state。
func markOrphaned(v KeyView, pools map[string]bool) KeyView {
	if pools != nil && !pools[v.Pool] {
		if v.StoredState == "" { // 已过期的 Key 已经记下了库中状态
			v.StoredState = v.State
		}
		v.State = KeyStateOrphaned
	}
	return v
}

func buildKeyViewWithStatus(k *store.UpstreamKey, st *scheduler.KeyStatus) KeyView {
	v := KeyView{
		ID:          k.ID,
		Pool:        k.Pool,
		Name:        k.Name,
		SecretHint:  k.SecretHint,
		State:       k.State,
		StateReason: k.StateReason,
		Weight:      k.Weight,
		MaxInFlight: k.MaxInFlight,
		Breaker:     "closed",
		Label:       k.Label,
		CreatedAt:   k.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   k.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if st != nil {
		v.InFlight = st.InFlight
		if st.CooldownRemaining > 0 {
			v.CooldownSeconds = int(math.Ceil(st.CooldownRemaining.Seconds()))
		}
		v.Breaker = st.Breaker.String()
		if st.QuotaLimit > 0 { // ADM-25
			v.QuotaUsed, v.QuotaLimit = st.QuotaUsed, st.QuotaLimit
			if !st.QuotaResetAt.IsZero() {
				v.QuotaResetAt = st.QuotaResetAt.UTC().Format(time.RFC3339)
			}
		}
	}
	if !k.ExpiresAt.IsZero() {
		v.ExpiresAt = k.ExpiresAt.UTC().Format(time.RFC3339)
		// T62：过期只在内存中判断；只有库中为 ready 的 Key 显示为 expired，库中状态放进 stored_state
		if st != nil && st.Expired && v.State == store.KeyStateReady {
			v.StoredState = v.State
			v.State = KeyStateExpired
		}
	}
	// ADM-25：额度用尽同样只在内存中判断；过期优先显示
	if st != nil && st.QuotaExhausted && v.State == store.KeyStateReady {
		v.StoredState = v.State
		v.State = KeyStateExhausted
	}
	return v
}

// KeyStateExpired 是已过期 Key 的展示状态（T62）；不写库，延长过期时间后立即恢复。
const KeyStateExpired = "expired"

// KeyStateExhausted 是本期额度用尽的 Key 的展示状态（T63）；不写库，到重置时刻自动恢复。
const KeyStateExhausted = "exhausted"

// maxLabelLen 是来源标签的最大长度（字节）。
const maxLabelLen = 64

// maxReasonLen 是停用原因的最大长度（字节，ADM-27）。
const maxReasonLen = 200

func validLabel(l string) error {
	if len(l) > maxLabelLen {
		return apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("label must be at most %d bytes", maxLabelLen))
	}
	return nil
}

// parseExpiresAt 解析 RFC 3339 时间（T62）。
func parseExpiresAt(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, apperr.New(apperr.CodeInvalidRequest, "expires_at must be an RFC 3339 time, e.g. 2026-12-31T23:59:59+08:00")
	}
	if t.Unix() == 0 {
		return time.Time{}, apperr.New(apperr.CodeInvalidRequest, "expires_at must not be the Unix epoch")
	}
	return t, nil
}

// writeKeyConflict 返回 409，并带上已存在 Key 的 id，供导入命令更新元数据（T62）。查不到时省略 id。
func (s *server) writeKeyConflict(w http.ResponseWriter, r *http.Request, fingerprint string) {
	e := apperr.New(apperr.CodeConflict, "upstream key conflict")
	body := map[string]any{"code": string(e.Code), "message": e.Message}
	if keys, err := s.d.Store.ListUpstreamKeys(r.Context()); err == nil {
		for _, k := range keys {
			if k.Fingerprint == fingerprint {
				body["key_id"] = k.ID
				break
			}
		}
	} else {
		s.d.Logger.Warn("admin: list keys for conflict lookup failed", "error", err)
	}
	apperr.WriteJSON(w, e, map[string]any{"error": body})
}
