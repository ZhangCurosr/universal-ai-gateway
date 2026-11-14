package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/obs"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// maxImportItems 是单次批量导入的最大条数（ADM-23）。
const maxImportItems = 1000

// maxImportNamePrefix 是名称前缀的最大长度：留出「-」与至少 7 位编号，保证名称不超过 64 字节。
const maxImportNamePrefix = 56

// 批量导入每一条的结果（ADM-23）。
const (
	ImportCreated   = "created"
	ImportUpdated   = "updated"
	ImportDuplicate = "duplicate"
	ImportInvalid   = "invalid"
	ImportFailed    = "failed"
)

// ImportKeysRequest 是 POST /admin/api/keys/import 的请求体（ADM-23）。
type ImportKeysRequest struct {
	Pool           string          `json:"pool"`
	NamePrefix     string          `json:"name_prefix"` // 空 = 池名
	Weight         *int            `json:"weight"`
	MaxInFlight    *int            `json:"max_in_flight"`
	UpdateExisting bool            `json:"update_existing"`
	Items          []ImportKeyItem `json:"items"`
}

// ImportKeyItem 是批量导入的一条。ExpiresAt、Label 省略表示「不设置」；
// update_existing 时，出现的字段会写到已有 Key 上（ExpiresAt 为空字符串 = 清除过期时间）。
type ImportKeyItem struct {
	Secret    string  `json:"secret"`
	ExpiresAt *string `json:"expires_at"`
	Label     *string `json:"label"`
}

// ImportKeyResult 是一条的处理结果；永远不包含密钥明文（ADM-24）。
type ImportKeyResult struct {
	Index      int    `json:"index"`
	Status     string `json:"status"`
	KeyID      int64  `json:"key_id,omitempty"`
	Name       string `json:"name,omitempty"`
	SecretHint string `json:"secret_hint,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ImportKeysResponse 是批量导入的响应。
type ImportKeysResponse struct {
	Created   int               `json:"created"`
	Updated   int               `json:"updated"`
	Duplicate int               `json:"duplicate"`
	Invalid   int               `json:"invalid"`
	Failed    int               `json:"failed"`
	Results   []ImportKeyResult `json:"results"`
}

func (s *server) handleImportKeys(w http.ResponseWriter, r *http.Request) {
	var req ImportKeysRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if !s.configuredPools()[req.Pool] {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("pool %q is not configured", req.Pool)))
		return
	}
	if len(req.Items) == 0 || len(req.Items) > maxImportItems {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("items must contain 1 to %d keys", maxImportItems)))
		return
	}
	prefix := strings.TrimSpace(req.NamePrefix)
	if prefix == "" {
		prefix = req.Pool
	}
	if len(prefix) > maxImportNamePrefix {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("name_prefix must be at most %d bytes", maxImportNamePrefix)))
		return
	}
	weight, maxInFlight := 1, 16
	if req.Weight != nil {
		if *req.Weight <= 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "weight must be > 0"))
			return
		}
		weight = *req.Weight
	}
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

	existing, err := s.d.Store.ListUpstreamKeys(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	byFingerprint := make(map[string]int64, len(existing))
	seq := 0
	for _, k := range existing {
		byFingerprint[k.Fingerprint] = k.ID
		if k.Pool == req.Pool {
			if n, ok := nameSeq(k.Name, prefix); ok && n > seq {
				seq = n
			}
		}
	}

	resp := ImportKeysResponse{Results: make([]ImportKeyResult, 0, len(req.Items))}
	changed := false
	for i, it := range req.Items {
		res := s.importOne(r, &req, it, prefix, weight, maxInFlight, byFingerprint, &seq)
		res.Index = i
		switch res.Status {
		case ImportCreated:
			resp.Created++
			changed = true
		case ImportUpdated:
			resp.Updated++
			changed = true
		case ImportDuplicate:
			resp.Duplicate++
		case ImportInvalid:
			resp.Invalid++
		default:
			resp.Failed++
		}
		resp.Results = append(resp.Results, res)
	}

	// ADM-23：整批只重载一次（ADM-14 的同步重载），并且只在确有变化时
	if changed && s.d.Scheduler != nil {
		loaded, err := scheduler.LoadKeys(r.Context(), s.d.Store, s.d.Keyring, s.d.Logger)
		if err != nil {
			s.d.Logger.Error("admin: reload keys failed after import", "pool", req.Pool, "error", err)
			writeError(w, apperr.Wrap(apperr.CodeInternal, "keys were written but reloading the scheduler failed", err))
			return
		}
		s.d.Scheduler.SetKeys(loaded)
	}
	// ADM-24：只记池名与数量
	s.d.Logger.Info("admin: upstream keys imported", "pool", req.Pool, "items", len(req.Items),
		"created", resp.Created, "updated", resp.Updated, "duplicate", resp.Duplicate, "invalid", resp.Invalid, "failed", resp.Failed)
	writeJSON(w, http.StatusOK, resp)
}

// importOne 处理一条；错误信息只描述原因，不含密钥（ADM-24）。
func (s *server) importOne(r *http.Request, req *ImportKeysRequest, it ImportKeyItem, prefix string, weight, maxInFlight int,
	byFingerprint map[string]int64, seq *int) ImportKeyResult {
	if len(it.Secret) == 0 || len(it.Secret) > 4096 {
		return ImportKeyResult{Status: ImportInvalid, Error: "secret must be non-empty and at most 4096 bytes"}
	}
	res := ImportKeyResult{SecretHint: obs.MaskSecret(it.Secret)}
	invalid := func(err error) ImportKeyResult {
		res.Status = ImportInvalid
		var ae *apperr.Error
		if errors.As(err, &ae) {
			res.Error = ae.Message
		} else {
			res.Error = "invalid item"
		}
		return res
	}
	var expiresAt time.Time
	if it.ExpiresAt != nil && *it.ExpiresAt != "" {
		t, err := parseExpiresAt(*it.ExpiresAt)
		if err != nil {
			return invalid(err)
		}
		expiresAt = t
	}
	label := ""
	if it.Label != nil {
		if err := validLabel(*it.Label); err != nil {
			return invalid(err)
		}
		label = *it.Label
	}

	fingerprint := auth.HashToken(s.d.Keyring.TokenPepper(), it.Secret)
	if id, ok := byFingerprint[fingerprint]; ok {
		return s.importExisting(r, req, it, expiresAt, id, res)
	}

	*seq++
	name := fmt.Sprintf("%s-%03d", prefix, *seq)
	k := &store.UpstreamKey{
		Pool:        req.Pool,
		Name:        name,
		Fingerprint: fingerprint,
		SecretHint:  res.SecretHint,
		Weight:      weight,
		MaxInFlight: maxInFlight,
		State:       store.KeyStateReady,
		ExpiresAt:   expiresAt,
		Label:       label,
	}
	seal := func(id int64) (crypto.Sealed, error) {
		return s.d.Keyring.Seal([]byte(it.Secret), crypto.UpstreamKeyAAD(id, req.Pool, s.d.Keyring.Version()))
	}
	id, err := s.d.Store.InsertUpstreamKey(r.Context(), k, seal)
	if err != nil {
		*seq-- // 没有写入，编号留给下一条
		if errors.Is(err, store.ErrConflict) {
			res.Status = ImportDuplicate
			return res
		}
		s.d.Logger.Error("admin: import key failed", "pool", req.Pool, "error", err)
		res.Status, res.Error = ImportFailed, "storage error"
		return res
	}
	byFingerprint[fingerprint] = id // 同一批里再次出现时按重复处理
	res.Status, res.KeyID, res.Name = ImportCreated, id, name
	return res
}

// importExisting 处理已存在的 Key：默认计为重复；update_existing 时写入出现的过期时间与标签。
func (s *server) importExisting(r *http.Request, req *ImportKeysRequest, it ImportKeyItem, expiresAt time.Time, id int64, res ImportKeyResult) ImportKeyResult {
	res.KeyID = id
	if !req.UpdateExisting || (it.ExpiresAt == nil && it.Label == nil) {
		res.Status = ImportDuplicate
		return res
	}
	upd := store.UpstreamKeyUpdate{Label: it.Label}
	if it.ExpiresAt != nil {
		upd.ExpiresAt = &expiresAt // 空字符串时为零值：清除过期时间
	}
	if err := s.d.Store.UpdateUpstreamKeyMeta(r.Context(), id, upd); err != nil {
		s.d.Logger.Error("admin: import key update failed", "key_id", id, "error", err)
		res.Status, res.Error = ImportFailed, "storage error"
		return res
	}
	res.Status = ImportUpdated
	return res
}

// nameSeq 解析 "<prefix>-<数字>" 形式的名称中的编号。
func nameSeq(name, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(name, prefix+"-")
	if !ok || rest == "" {
		return 0, false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}
