package scheduler

import (
	"context"
	"log/slog"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// KeyLister 是 LoadKeys 需要的最小存储能力。
type KeyLister interface {
	ListUpstreamKeys(ctx context.Context) ([]store.UpstreamKey, error)
}

// LoadKeys 从存储读取全部上游 Key 并解密（任务卡 T08 第 3 步）。
// 单个 Key 解密失败时记录 Error 日志并跳过，不返回错误。
func LoadKeys(ctx context.Context, st KeyLister, kr *crypto.Keyring, log *slog.Logger) ([]*Key, error) {
	rows, err := st.ListUpstreamKeys(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]*Key, 0, len(rows))
	for _, k := range rows {
		secret, err := kr.Open(k.Sealed, crypto.UpstreamKeyAAD(k.ID, k.Pool, k.Sealed.KEKVersion))
		if err != nil {
			// 只记录定位信息，不记录密文与明文
			log.Error("scheduler: cannot decrypt upstream key; skipped", "key_id", k.ID, "pool", k.Pool, "name", k.Name, "error", err)
			continue
		}
		keys = append(keys, NewKey(k.ID, k.Pool, k.Name, secret, k.Weight, k.MaxInFlight, ParsePersistedState(k.State)))
	}
	return keys, nil
}
