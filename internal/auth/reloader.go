package auth

import (
	"context"
	"fmt"
	"sync"

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

	mu sync.Mutex // 串行化 Reload：防止较早读到的旧快照在较新快照之后才 Replace（ADM-12 轮换后旧 Token 必须立即失效）
}

// Reload 读取全部客户端并替换索引。
func (r *Reloader) Reload(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clients, err := r.Store.ListClients(ctx)
	if err != nil {
		return fmt.Errorf("reload clients: %w", err)
	}
	r.Index.Replace(clients)
	return nil
}
