package scheduler

import (
	"context"
	"log/slog"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
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
	// TODO(T08): ListUpstreamKeys → kr.Open(k.Sealed, crypto.UpstreamKeyAAD(k.ID, k.Pool, k.Sealed.KEKVersion)) → NewKey。
	return nil, apperr.NotImplemented("T08")
}
