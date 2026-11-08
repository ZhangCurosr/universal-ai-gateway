package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// InsertAuditRecords 实现 STO-13。
func (d *DB) InsertAuditRecords(ctx context.Context, recs []store.AuditRecord) error {
	if len(recs) == 0 {
		return nil
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin insert audit records tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	const query = `
		INSERT INTO audit_records (
			request_id, client_id, key_id, model, op,
			ingress_protocol, upstream_protocol, status, error_code, outcome,
			attempts, stream, ttfb_ms, latency_ms, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("prepare insert audit record stmt: %w", err)
	}
	defer stmt.Close()

	for i := range recs {
		r := &recs[i]
		createdAt := r.CreatedAt.UnixMilli()
		if r.CreatedAt.IsZero() {
			createdAt = d.now().UnixMilli()
		}

		if _, err := stmt.ExecContext(ctx,
			r.RequestID,
			r.ClientID,
			r.KeyID,
			r.Model,
			r.Op,
			r.IngressProtocol,
			r.UpstreamProtocol,
			r.Status,
			r.ErrorCode,
			r.Outcome,
			r.Attempts,
			boolToInt(r.Stream),
			r.TTFBMillis,
			r.LatencyMillis,
			createdAt,
		); err != nil {
			return fmt.Errorf("insert audit record %s: %w", r.RequestID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit audit records: %w", err)
	}
	return nil
}

// DeleteAuditBefore 实现 STO-14。
func (d *DB) DeleteAuditBefore(ctx context.Context, before time.Time) (int64, error) {
	const query = `DELETE FROM audit_records WHERE created_at < ?`
	res, err := d.db.ExecContext(ctx, query, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("delete audit before: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("get delete audit rows affected: %w", err)
	}
	return affected, nil
}
