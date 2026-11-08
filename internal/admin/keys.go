package admin

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func (s *server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req CreateKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	// ADM-13: pool 必须在配置中存在（否则 400）
	poolFound := false
	if s.d.Config != nil {
		for _, p := range s.d.Config.Pools {
			if p.Name == req.Pool {
				poolFound = true
				break
			}
		}
	}
	if !poolFound {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("pool %q is not configured", req.Pool)))
		return
	}

	name := strings.TrimSpace(req.Name)
	if len(name) == 0 || len(req.Name) > 64 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "name must be between 1 and 64 characters"))
		return
	}

	// ADM-13: secret 非空、<= 4096 字节
	if len(req.Secret) == 0 || len(req.Secret) > 4096 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "secret must be non-empty and at most 4096 bytes"))
		return
	}

	weight := 1
	if req.Weight != nil {
		if *req.Weight <= 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "weight must be > 0"))
			return
		}
		weight = *req.Weight
	}

	maxInFlight := 16
	if req.MaxInFlight != nil {
		if *req.MaxInFlight <= 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "max_in_flight must be > 0"))
			return
		}
		maxInFlight = *req.MaxInFlight
	}

	if s.d.Keyring == nil {
		writeError(w, apperr.New(apperr.CodeInternal, "keyring not available"))
		return
	}

	// ADM-13: fingerprint = HashToken(pepper, secret)
	fingerprint := auth.HashToken(s.d.Keyring.TokenPepper(), req.Secret)
	secretHint := obs.MaskSecret(req.Secret)

	seal := func(id int64) (crypto.Sealed, error) {
		aad := crypto.UpstreamKeyAAD(id, req.Pool, s.d.Keyring.Version())
		return s.d.Keyring.Seal([]byte(req.Secret), aad)
	}

	k := &store.UpstreamKey{
		Pool:        req.Pool,
		Name:        name,
		Fingerprint: fingerprint,
		SecretHint:  secretHint,
		Weight:      weight,
		MaxInFlight: maxInFlight,
		State:       store.KeyStateReady,
	}

	id, err := s.d.Store.InsertUpstreamKey(r.Context(), k, seal)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, apperr.New(apperr.CodeConflict, "upstream key conflict"))
			return
		}
		writeError(w, err)
		return
	}
	k.ID = id

	// ADM-14: 创建 Key 后，同步执行 scheduler.LoadKeys + Scheduler.SetKeys
	if s.d.Scheduler != nil {
		loadedKeys, err := scheduler.LoadKeys(r.Context(), s.d.Store, s.d.Keyring, s.d.Logger)
		if err != nil {
			s.d.Logger.Error("admin: reload keys failed after create", "key_id", id, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "failed to reload scheduler keys", err))
			return
		}
		s.d.Scheduler.SetKeys(loadedKeys)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"key": s.buildKeyView(k),
	})
}

func (s *server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.d.Store.ListUpstreamKeys(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}

	stMap := s.keyStatusMap()
	views := make([]KeyView, 0, len(keys))
	for i := range keys {
		st, ok := stMap[keys[i].ID]
		var stPtr *scheduler.KeyStatus
		if ok {
			stPtr = &st
		}
		views = append(views, buildKeyViewWithStatus(&keys[i], stPtr))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"keys": views,
	})
}

func (s *server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}

	_, err = s.d.Store.GetUpstreamKey(r.Context(), id) // 先确认存在，不存在直接 404，避免部分写入
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	var req UpdateKeyRequest
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

	if req.Weight != nil && *req.Weight <= 0 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "weight must be > 0"))
		return
	}

	if req.MaxInFlight != nil && *req.MaxInFlight <= 0 {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "max_in_flight must be > 0"))
		return
	}

	// ADM-15: PATCH keys 的 state 只接受 ready / disabled；设为 ready 会清除 quarantined 状态与 state_reason
	if req.State != nil {
		if *req.State != store.KeyStateReady && *req.State != store.KeyStateDisabled {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "state must be ready or disabled"))
			return
		}
	}

	if req.Name != nil || req.Weight != nil || req.MaxInFlight != nil {
		err := s.d.Store.UpdateUpstreamKeyMeta(r.Context(), id, store.UpstreamKeyUpdate{
			Name:        req.Name,
			Weight:      req.Weight,
			MaxInFlight: req.MaxInFlight,
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
				return
			}
			writeError(w, err)
			return
		}
	}

	if req.State != nil {
		state := *req.State
		reason := ""
		if state == store.KeyStateDisabled {
			reason = "disabled by admin"
		}
		// state 为 ready 时，reason 为 ""，清除 quarantined 状态与 state_reason (ADM-15)
		err := s.d.Store.SetUpstreamKeyState(r.Context(), id, state, reason)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
				return
			}
			writeError(w, err)
			return
		}
	}

	// ADM-14: 修改 Key 后，同步执行 scheduler.LoadKeys + Scheduler.SetKeys
	if s.d.Scheduler != nil && s.d.Keyring != nil {
		loadedKeys, err := scheduler.LoadKeys(r.Context(), s.d.Store, s.d.Keyring, s.d.Logger)
		if err != nil {
			s.d.Logger.Error("admin: reload keys failed after patch", "key_id", id, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "failed to reload scheduler keys", err))
			return
		}
		s.d.Scheduler.SetKeys(loadedKeys)
	}

	updated, err := s.d.Store.GetUpstreamKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, apperr.New(apperr.CodeNotFound, fmt.Sprintf("upstream key %d not found", id)))
			return
		}
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"key": s.buildKeyView(updated),
	})
}

func (s *server) keyStatusMap() map[int64]scheduler.KeyStatus {
	m := make(map[int64]scheduler.KeyStatus)
	if s.d.Scheduler != nil {
		for _, st := range s.d.Scheduler.KeyStatus() {
			m[st.ID] = st
		}
	}
	return m
}

func (s *server) buildKeyView(k *store.UpstreamKey) KeyView {
	stMap := s.keyStatusMap()
	st, ok := stMap[k.ID]
	var stPtr *scheduler.KeyStatus
	if ok {
		stPtr = &st
	}
	return buildKeyViewWithStatus(k, stPtr)
}

func buildKeyViewWithStatus(k *store.UpstreamKey, st *scheduler.KeyStatus) KeyView {
	v := KeyView{
		ID:          k.ID,
		Pool:        k.Pool,
		Name:        k.Name,
		SecretHint:  k.SecretHint,
		State:       k.State,
		StateReason: k.StateReason,
		Weight:      k.Weight,
		MaxInFlight: k.MaxInFlight,
		Breaker:     "closed",
		CreatedAt:   k.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   k.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if st != nil {
		v.InFlight = st.InFlight
		if st.CooldownRemaining > 0 {
			v.CooldownSeconds = int(math.Ceil(st.CooldownRemaining.Seconds()))
		}
		v.Breaker = st.Breaker.String()
	}
	return v
}
