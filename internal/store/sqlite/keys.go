package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

const upstreamKeyColumns = `id, pool, name, fingerprint, secret_hint, kek_version, wrapped_dek, ciphertext, weight, max_in_flight, state, state_reason, created_at, updated_at`

func scanUpstreamKey(s rowScanner) (*store.UpstreamKey, error) {
	var (
		k           store.UpstreamKey
		createdAtMs int64
		updatedAtMs int64
	)
	err := s.Scan(
		&k.ID,
		&k.Pool,
		&k.Name,
		&k.Fingerprint,
		&k.SecretHint,
		&k.Sealed.KEKVersion,
		&k.Sealed.WrappedDEK,
		&k.Sealed.Ciphertext,
		&k.Weight,
		&k.MaxInFlight,
		&k.State,
		&k.StateReason,
		&createdAtMs,
		&updatedAtMs,
	)
	if err != nil {
		return nil, err
	}
	k.CreatedAt = fromMillis(createdAtMs)
	k.UpdatedAt = fromMillis(updatedAtMs)
	return &k, nil
}

// InsertUpstreamKey 实现 STO-06：单事务内 插入占位行 → seal(id) → 写入密文 → 提交。
func (d *DB) InsertUpstreamKey(ctx context.Context, k *store.UpstreamKey, seal store.SealFunc) (int64, error) {
	weight := k.Weight
	if weight <= 0 {
		weight = 1
	}
	maxInFlight := k.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = 16
	}
	state := k.State
	if state == "" {
		state = store.KeyStateReady
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin insert upstream key tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := d.now().UnixMilli()
	const insertQuery = `
		INSERT INTO upstream_keys (
			pool, name, fingerprint, secret_hint,
			kek_version, wrapped_dek, ciphertext,
			weight, max_in_flight, state, state_reason,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, 0, x'', x'', ?, ?, ?, ?, ?, ?)`

	res, err := tx.ExecContext(ctx, insertQuery,
		k.Pool,
		k.Name,
		k.Fingerprint,
		k.SecretHint,
		weight,
		maxInFlight,
		state,
		k.StateReason,
		now,
		now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, store.ErrConflict
		}
		return 0, fmt.Errorf("insert upstream key placeholder: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("get last insert id: %w", err)
	}

	sealed, err := seal(id)
	if err != nil {
		return 0, err
	}

	const updateQuery = `
		UPDATE upstream_keys
		SET kek_version = ?, wrapped_dek = ?, ciphertext = ?
		WHERE id = ?`

	if _, err := tx.ExecContext(ctx, updateQuery, sealed.KEKVersion, sealed.WrappedDEK, sealed.Ciphertext, id); err != nil {
		return 0, fmt.Errorf("update sealed upstream key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit upstream key: %w", err)
	}

	k.ID = id
	k.Sealed = sealed
	k.CreatedAt = fromMillis(now)
	k.UpdatedAt = fromMillis(now)
	return id, nil
}

// ListUpstreamKeys 实现 STO-07。
func (d *DB) ListUpstreamKeys(ctx context.Context) ([]store.UpstreamKey, error) {
	const query = `SELECT ` + upstreamKeyColumns + ` FROM upstream_keys ORDER BY id ASC`
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list upstream keys: %w", err)
	}
	defer rows.Close()

	keys := make([]store.UpstreamKey, 0)
	for rows.Next() {
		k, err := scanUpstreamKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan upstream key: %w", err)
		}
		keys = append(keys, *k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate upstream keys: %w", err)
	}
	return keys, nil
}

// GetUpstreamKey 返回单个 Key；不存在返回 store.ErrNotFound。
func (d *DB) GetUpstreamKey(ctx context.Context, id int64) (*store.UpstreamKey, error) {
	const query = `SELECT ` + upstreamKeyColumns + ` FROM upstream_keys WHERE id = ?`
	row := d.db.QueryRowContext(ctx, query, id)
	k, err := scanUpstreamKey(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get upstream key %d: %w", id, err)
	}
	return k, nil
}

// UpdateUpstreamKeyMeta 实现 STO-08。
func (d *DB) UpdateUpstreamKeyMeta(ctx context.Context, id int64, upd store.UpstreamKeyUpdate) error {
	setClauses := make([]string, 0, 4)
	args := make([]any, 0, 5)

	if upd.Name != nil {
		setClauses = append(setClauses, "name = ?")
		args = append(args, *upd.Name)
	}
	if upd.Weight != nil {
		setClauses = append(setClauses, "weight = ?")
		args = append(args, *upd.Weight)
	}
	if upd.MaxInFlight != nil {
		setClauses = append(setClauses, "max_in_flight = ?")
		args = append(args, *upd.MaxInFlight)
	}

	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, d.now().UnixMilli())
	args = append(args, id)

	query := fmt.Sprintf("UPDATE upstream_keys SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	res, err := d.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update upstream key meta %d: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected for upstream key %d: %w", id, err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// SetUpstreamKeyState 实现 STO-09。
func (d *DB) SetUpstreamKeyState(ctx context.Context, id int64, state, reason string) error {
	switch state {
	case store.KeyStateReady, store.KeyStateQuarantined, store.KeyStateDisabled:
	default:
		return store.ErrInvalidArgument
	}

	now := d.now().UnixMilli()
	const query = `UPDATE upstream_keys SET state = ?, state_reason = ?, updated_at = ? WHERE id = ?`
	res, err := d.db.ExecContext(ctx, query, state, reason, now, id)
	if err != nil {
		return fmt.Errorf("set upstream key state %d: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected for upstream key %d: %w", id, err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}
