package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

const clientColumns = `id, name, role, token_hash, token_display, quota_unlimited, quota_nano_usd, rpm_limit, allowed_models, disabled, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanClient(s rowScanner) (*store.Client, error) {
	var (
		c                 store.Client
		quotaUnlimited    int64
		disabled          int64
		allowedModelsJSON string
		createdAtMs       int64
		updatedAtMs       int64
	)
	err := s.Scan(
		&c.ID,
		&c.Name,
		&c.Role,
		&c.TokenHash,
		&c.TokenDisplay,
		&quotaUnlimited,
		&c.QuotaNanoUSD,
		&c.RPMLimit,
		&allowedModelsJSON,
		&disabled,
		&createdAtMs,
		&updatedAtMs,
	)
	if err != nil {
		return nil, err
	}
	c.QuotaUnlimited = intToBool(quotaUnlimited)
	c.Disabled = intToBool(disabled)
	c.CreatedAt = fromMillis(createdAtMs)
	c.UpdatedAt = fromMillis(updatedAtMs)

	var models []string
	trimmed := strings.TrimSpace(allowedModelsJSON)
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &models); err != nil {
			return nil, fmt.Errorf("unmarshal allowed_models for client %d: %w", c.ID, err)
		}
	}
	if models == nil {
		models = []string{}
	}
	c.AllowedModels = models

	return &c, nil
}

// CreateClient 插入新客户端，实现 STO-01。
// 忽略 c.ID，created_at 与 updated_at 设置为当前时间。token_hash 冲突返回 store.ErrConflict。
func (d *DB) CreateClient(ctx context.Context, c *store.Client) (int64, error) {
	modelsJSON := "[]"
	if len(c.AllowedModels) > 0 {
		b, err := json.Marshal(c.AllowedModels)
		if err != nil {
			return 0, fmt.Errorf("marshal allowed_models: %w", err)
		}
		modelsJSON = string(b)
	}

	now := d.now().UnixMilli()
	const query = `
		INSERT INTO clients (
			name, role, token_hash, token_display,
			quota_unlimited, quota_nano_usd, rpm_limit,
			allowed_models, disabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	res, err := d.db.ExecContext(ctx, query,
		c.Name,
		c.Role,
		c.TokenHash,
		c.TokenDisplay,
		boolToInt(c.QuotaUnlimited),
		c.QuotaNanoUSD,
		c.RPMLimit,
		modelsJSON,
		boolToInt(c.Disabled),
		now,
		now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, store.ErrConflict
		}
		return 0, fmt.Errorf("insert client: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get last insert id: %w", err)
	}
	return id, nil
}

// GetClient 根据 ID 查询客户端，实现 STO-02。
// 不存在返回 store.ErrNotFound。
func (d *DB) GetClient(ctx context.Context, id int64) (*store.Client, error) {
	const query = `SELECT ` + clientColumns + ` FROM clients WHERE id = ?`
	row := d.db.QueryRowContext(ctx, query, id)
	c, err := scanClient(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("query client %d: %w", id, err)
	}
	return c, nil
}

// ListClients 按 ID 升序返回全部客户端，实现 STO-03。
func (d *DB) ListClients(ctx context.Context) ([]store.Client, error) {
	const query = `SELECT ` + clientColumns + ` FROM clients ORDER BY id ASC`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list clients: %w", err)
	}
	defer rows.Close()

	clients := make([]store.Client, 0)
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, fmt.Errorf("scan client: %w", err)
		}
		clients = append(clients, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clients: %w", err)
	}
	return clients, nil
}

// UpdateClient 更新客户端属性并刷新 updated_at，实现 STO-04。
// 不存在返回 store.ErrNotFound。
func (d *DB) UpdateClient(ctx context.Context, c *store.Client) error {
	modelsJSON := "[]"
	if len(c.AllowedModels) > 0 {
		b, err := json.Marshal(c.AllowedModels)
		if err != nil {
			return fmt.Errorf("marshal allowed_models: %w", err)
		}
		modelsJSON = string(b)
	}

	now := d.now().UnixMilli()
	const query = `
		UPDATE clients SET
			name = ?,
			role = ?,
			token_hash = ?,
			token_display = ?,
			quota_unlimited = ?,
			quota_nano_usd = ?,
			rpm_limit = ?,
			allowed_models = ?,
			disabled = ?,
			updated_at = ?
		WHERE id = ?`

	res, err := d.db.ExecContext(ctx, query,
		c.Name,
		c.Role,
		c.TokenHash,
		c.TokenDisplay,
		boolToInt(c.QuotaUnlimited),
		c.QuotaNanoUSD,
		c.RPMLimit,
		modelsJSON,
		boolToInt(c.Disabled),
		now,
		c.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrConflict
		}
		return fmt.Errorf("update client %d: %w", c.ID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected for client %d: %w", c.ID, err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	c.UpdatedAt = fromMillis(now)
	return nil
}

// CountAdmins 统计未禁用的管理员数量，实现 STO-05。
func (d *DB) CountAdmins(ctx context.Context) (int, error) {
	const query = `SELECT COUNT(*) FROM clients WHERE role = ? AND disabled = 0`
	var count int
	if err := d.db.QueryRowContext(ctx, query, store.RoleAdmin).Scan(&count); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return count, nil
}
