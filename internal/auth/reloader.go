package auth

import (
	"context"
	"fmt"

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
	clients, err := r.Store.ListClients(ctx)
	if err != nil {
		return fmt.Errorf("reload clients: %w", err)
	}
	r.Index.Replace(clients)
	return nil
}
