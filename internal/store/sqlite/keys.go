package sqlite

import (
	"context"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// InsertUpstreamKey 实现 STO-06：单事务内 插入占位行 → seal(id) → 写入密文 → 提交。
func (d *DB) InsertUpstreamKey(ctx context.Context, k *store.UpstreamKey, seal store.SealFunc) (int64, error) {
	// TODO(T02): 严格按 STO-06 四步实现；seal 出错回滚并原样返回；fingerprint 冲突返回 store.ErrConflict。
	return 0, apperr.NotImplemented("T02")
}

// ListUpstreamKeys 实现 STO-07。
func (d *DB) ListUpstreamKeys(ctx context.Context) ([]store.UpstreamKey, error) {
	// TODO(T02): 按 id 升序返回全部，含密文字段。
	return nil, apperr.NotImplemented("T02")
}

// GetUpstreamKey 返回单个 Key；不存在返回 store.ErrNotFound。
func (d *DB) GetUpstreamKey(ctx context.Context, id int64) (*store.UpstreamKey, error) {
	// TODO(T02)
	return nil, apperr.NotImplemented("T02")
}

// UpdateUpstreamKeyMeta 实现 STO-08。
func (d *DB) UpdateUpstreamKeyMeta(ctx context.Context, id int64, upd store.UpstreamKeyUpdate) error {
	// TODO(T02): 只更新非 nil 字段；不存在返回 store.ErrNotFound。
	return apperr.NotImplemented("T02")
}

// SetUpstreamKeyState 实现 STO-09。
func (d *DB) SetUpstreamKeyState(ctx context.Context, id int64, state, reason string) error {
	// TODO(T02): 校验 state 取值，非法返回 store.ErrInvalidArgument；不存在返回 store.ErrNotFound。
	return apperr.NotImplemented("T02")
}
