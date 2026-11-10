package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	now := s.d.Now()
	var uptime int64
	if !s.d.StartedAt.IsZero() && now.After(s.d.StartedAt) {
		uptime = int64(now.Sub(s.d.StartedAt).Seconds())
	}
	out := map[string]any{
		"status":         "ok",
		"version":        s.d.Version,
		"uptime_seconds": uptime,
	}
	// spec/09 §6.4：附加字段只在注入了取数函数时输出
	if s.d.LedgerQueue != nil {
		length, capacity := s.d.LedgerQueue()
		out["ledger_queue"] = map[string]int{"length": length, "capacity": capacity}
	}
	if s.d.AuditDropped != nil {
		out["audit_dropped"] = s.d.AuditDropped()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var req CreateClientRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	name := strings.TrimSpace(req.Name)
	if len(name) == 0 || len(req.Name) > 64 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "name must be between 1 and 64 characters"))
		return
	}

	role := req.Role
	if role == "" {
		role = store.RoleUser
	}
	if role != store.RoleUser && role != store.RoleAdmin {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "role must be user or admin"))
		return
	}

	if !req.QuotaUnlimited && req.QuotaNanoUSD < 0 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "quota_nano_usd must be >= 0"))
		return
	}

	rpm := 60
	if req.RPMLimit != nil {
		if *req.RPMLimit < 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "rpm_limit must be >= 0"))
			return
		}
		rpm = *req.RPMLimit
	}

	allowed, err := s.normalizeAllowedModels(req.AllowedModels)
	if err != nil {
		writeError(w, err)
		return
	}

	plainToken, display, err := auth.GenerateToken()
	if err != nil {
		writeError(w, err)
		return
	}

	if s.d.Keyring == nil {
		writeError(w, apperr.New(apperr.CodeInternal, "keyring not available"))
		return
	}

	tokenHash := auth.HashToken(s.d.Keyring.TokenPepper(), plainToken)
	c := &store.Client{
		Name:           name,
		Role:           role,
		TokenHash:      tokenHash,
		TokenDisplay:   display,
		QuotaUnlimited: req.QuotaUnlimited,
		QuotaNanoUSD:   req.QuotaNanoUSD,
		RPMLimit:       rpm,
		AllowedModels:  allowed,
		Disabled:       false,
	}

	id, err := s.d.Store.CreateClient(r.Context(), c)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, apperr.New(apperr.CodeConflict, "client conflict"))
			return
		}
		writeError(w, err)
		return
	}
	c.ID = id

	// ADM-10: 同步执行 auth.Reloader.Reload 与 ledger.UpsertAccount
	if s.d.Reloader != nil {
		if err := s.d.Reloader.Reload(r.Context()); err != nil {
			s.d.Logger.Error("admin: reload index failed after client create", "client_id", id, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "failed to reload auth index", err))
			return
		}
	}
	if s.d.Ledger != nil {
		s.d.Ledger.UpsertAccount(ledger.Account{
			ClientID:     id,
			Unlimited:    c.QuotaUnlimited,
			QuotaNanoUSD: c.QuotaNanoUSD,
			SpentNanoUSD: 0,
		})
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"client": buildClientView(c, s.d.Ledger),
		"token":  plainToken,
	})
}

func (s *server) handleListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.d.Store.ListClients(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}

	views := make([]ClientView, 0, len(clients))
	for i := range clients {
		views = append(views, buildClientView(&clients[i], s.d.Ledger))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"clients": views,
	})
}

func (s *server) handleGetClient(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	c, err := s.d.Store.GetClient(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("client %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"client": buildClientView(c, s.d.Ledger),
	})
}

func (s *server) handleUpdateClient(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	existing, err := s.d.Store.GetClient(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("client %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	var req UpdateClientRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if len(trimmed) == 0 || len(*req.Name) > 64 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "name must be between 1 and 64 characters"))
			return
		}
	}

	if req.Role != nil {
		if *req.Role != store.RoleUser && *req.Role != store.RoleAdmin {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "role must be user or admin"))
			return
		}
	}

	if req.QuotaNanoUSD != nil && *req.QuotaNanoUSD < 0 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "quota_nano_usd must be >= 0"))
		return
	}

	if req.RPMLimit != nil && *req.RPMLimit < 0 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "rpm_limit must be >= 0"))
		return
	}

	var allowed []string
	if req.AllowedModels != nil {
		var err error
		if allowed, err = s.normalizeAllowedModels(*req.AllowedModels); err != nil {
			writeError(w, err)
			return
		}
	}

	// ADM-03: 管理员不能禁用或降级最后一个未禁用的管理员（CountAdmins 检查）
	if existing.Role == store.RoleAdmin && !existing.Disabled {
		willDisable := req.Disabled != nil && *req.Disabled
		willDemote := req.Role != nil && *req.Role != store.RoleAdmin
		if willDisable || willDemote {
			count, err := s.d.Store.CountAdmins(r.Context())
			if err != nil {
				writeError(w, err)
				return
			}
			if count <= 1 {
				writeError(w, apperr.New(apperr.CodeConflict, "cannot disable or demote the last active admin"))
				return
			}
		}
	}

	if req.Name != nil {
		existing.Name = strings.TrimSpace(*req.Name)
	}
	if req.Role != nil {
		existing.Role = *req.Role
	}
	if req.QuotaUnlimited != nil {
		existing.QuotaUnlimited = *req.QuotaUnlimited
	}
	if req.QuotaNanoUSD != nil {
		existing.QuotaNanoUSD = *req.QuotaNanoUSD
	}
	if req.RPMLimit != nil {
		existing.RPMLimit = *req.RPMLimit
	}
	if req.AllowedModels != nil {
		existing.AllowedModels = allowed
	}
	if req.Disabled != nil {
		existing.Disabled = *req.Disabled
	}

	if err := s.d.Store.UpdateClient(r.Context(), existing); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, apperr.New(apperr.CodeConflict, "client conflict"))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("client %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	// ADM-10: 同步执行 auth.Reloader.Reload 与 ledger.UpsertAccount
	if s.d.Reloader != nil {
		if err := s.d.Reloader.Reload(r.Context()); err != nil {
			s.d.Logger.Error("admin: reload index failed after client update", "client_id", id, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "failed to reload auth index", err))
			return
		}
	}
	if s.d.Ledger != nil {
		s.d.Ledger.UpsertAccount(ledger.Account{
			ClientID:     existing.ID,
			Unlimited:    existing.QuotaUnlimited,
			QuotaNanoUSD: existing.QuotaNanoUSD,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"client": buildClientView(existing, s.d.Ledger),
	})
}

func (s *server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	c, err := s.d.Store.GetClient(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("client %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	plainToken, display, err := auth.GenerateToken()
	if err != nil {
		writeError(w, err)
		return
	}

	if s.d.Keyring == nil {
		writeError(w, apperr.New(apperr.CodeInternal, "keyring not available"))
		return
	}

	tokenHash := auth.HashToken(s.d.Keyring.TokenPepper(), plainToken)
	c.TokenHash = tokenHash
	c.TokenDisplay = display

	if err := s.d.Store.UpdateClient(r.Context(), c); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("client %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	// ADM-12: 轮转 Token，旧 Token 立即失效（Reload 之后）
	if s.d.Reloader != nil {
		if err := s.d.Reloader.Reload(r.Context()); err != nil {
			s.d.Logger.Error("admin: reload index failed after rotate-token", "client_id", id, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "failed to reload auth index", err))
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"client": buildClientView(c, s.d.Ledger),
		"token":  plainToken,
	})
}

func buildClientView(c *store.Client, l *ledger.Memory) ClientView {
	cv := ClientView{
		ID:             c.ID,
		Name:           c.Name,
		Role:           c.Role,
		TokenDisplay:   c.TokenDisplay,
		QuotaUnlimited: c.QuotaUnlimited,
		QuotaNanoUSD:   c.QuotaNanoUSD,
		RPMLimit:       c.RPMLimit,
		AllowedModels:  c.AllowedModels,
		Disabled:       c.Disabled,
		CreatedAt:      c.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      c.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if cv.AllowedModels == nil {
		cv.AllowedModels = []string{}
	}
	if l != nil {
		if snap, ok := l.Snapshot(c.ID); ok {
			cv.SpentNanoUSD = snap.SpentNanoUSD
			cv.ReservedNanoUSD = snap.ReservedNanoUSD
		}
	}
	return cv
}

// normalizeAllowedModels 校验允许列表中的每个模型都已配置，并去掉重复项（保持首次出现的顺序）。
// 空列表表示不限制，原样返回。
func (s *server) normalizeAllowedModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return models, nil
	}
	if s.d.Config == nil || len(s.d.Config.Models) == 0 {
		return nil, apperr.New(apperr.CodeInvalidRequest, "no models are configured")
	}
	configured := make(map[string]bool, len(s.d.Config.Models))
	for _, cm := range s.d.Config.Models {
		configured[cm.Name] = true
	}
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, m := range models {
		if !configured[m] {
			return nil, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("model %q is not configured", m))
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out, nil
}
