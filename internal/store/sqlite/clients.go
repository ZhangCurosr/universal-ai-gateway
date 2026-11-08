package sqlite

import (
	"context"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// CreateClient 实现 STO-01。
func (d *DB) CreateClient(ctx context.Context, c *store.Client) (int64, error) {
	// TODO(T01): 插入 clients 一行；allowed_models 存 JSON；token_hash 冲突返回 store.ErrConflict。
	return 0, apperr.NotImplemented("T01")
}

// GetClient 实现 STO-02。
func (d *DB) GetClient(ctx context.Context, id int64) (*store.Client, error) {
	// TODO(T01): 不存在返回 store.ErrNotFound。
	return nil, apperr.NotImplemented("T01")
}

// ListClients 实现 STO-03。
func (d *DB) ListClients(ctx context.Context) ([]store.Client, error) {
	// TODO(T01): 按 id 升序返回全部客户端。
	return nil, apperr.NotImplemented("T01")
}

// UpdateClient 实现 STO-04。
func (d *DB) UpdateClient(ctx context.Context, c *store.Client) error {
	// TODO(T01): 按 c.ID 更新可变字段并刷新 updated_at；不存在返回 store.ErrNotFound。
	return apperr.NotImplemented("T01")
}

// CountAdmins 实现 STO-05。
func (d *DB) CountAdmins(ctx context.Context) (int, error) {
	// TODO(T01): 统计 role='admin' AND disabled=0。
	return 0, apperr.NotImplemented("T01")
}
