package auth

import (
	"context"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// ClientLister 是 Reloader 需要的最小存储能力。
type ClientLister interface {
	ListClients(ctx context.Context) ([]store.Client, error)
}

// Reloader 从存储重新加载客户端到 Index（spec/05 §3.4）。
type Reloader struct {
	Store ClientLister
	Index *Index
}

// Reload 读取全部客户端并替换索引。
func (r *Reloader) Reload(ctx context.Context) error {
	// TODO(T03): ListClients → Index.Replace；错误用 fmt.Errorf("reload clients: %w", err) 包装。
	return apperr.NotImplemented("T03")
}
