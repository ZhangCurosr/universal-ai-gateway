package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/files"
)

// uploadReadPerMiB 是 FS-10 上传时每 MiB 追加的读取时限（约 100 KiB/s 的最低速度）。
const uploadReadPerMiB = 10 * time.Second

// fileBody 实现 FS-20 / FS-22 中直接 HTTP 请求的一侧，套在鉴权与客户端限流之后。
func (d DataDeps) fileBody(ew ErrorWriter, allowBody bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !files.Wants(r) && r.Header.Get(files.HeaderFileNameQuery) == "" {
				next.ServeHTTP(w, r)
				return
			}
			p, _ := auth.PrincipalFrom(r.Context())
			var owner int64
			if p != nil {
				owner = p.ClientID
			}
			cleanup, err := files.Attach(d.Files, r, owner, allowBody)
			if err != nil {
				writeErr(w, r, ew, err)
				return
			}
			defer cleanup()
			next.ServeHTTP(w, r)
		})
	}
}

// fileBudget 放在过载保护之前：带 X-UAIG-File 的请求按未知长度占用请求体预算（OVL-02 取单个请求的上限），
// 因为替换后的请求体在鉴权之后才知道大小。
func fileBudget(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if files.Wants(r) {
			r.ContentLength = -1
		}
		next.ServeHTTP(w, r)
	})
}

// mountFiles 注册 FS-10 / FS-11 的路由（FS-12：与 /mcp 相同的中间件，错误格式用兜底协议）。
func (d DataDeps) mountFiles(mux *http.ServeMux, ew ErrorWriter) {
	wrap := func(h http.HandlerFunc) http.Handler {
		return chain(h,
			IPRateLimit(d.IPLimiter, d.Config.Limits.PreAuthIPRPM, ew),
			d.Overload.Middleware(ew, false),
			Authenticate(d.Index, d.Pepper, ew),
			RateLimit(d.Limiter, ew),
		)
	}
	owner := func(r *http.Request) int64 {
		if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
			return p.ClientID
		}
		return 0
	}
	fail := func(w http.ResponseWriter, r *http.Request, err error) { writeErr(w, r, ew, err) }

	mux.Handle("POST /files", wrap(func(w http.ResponseWriter, r *http.Request) {
		s := d.Files
		name, err := files.CleanName(r.URL.Query().Get("name"))
		if err != nil {
			fail(w, r, err)
			return
		}
		var ttl time.Duration
		if v := r.URL.Query().Get("ttl"); v != "" {
			if ttl, err = time.ParseDuration(v); err != nil {
				fail(w, r, apperr.New(apperr.CodeInvalidRequest, "ttl must be a duration such as 30m or 24h"))
				return
			}
		}
		if ttl, err = s.TTL(ttl); err != nil {
			fail(w, r, err)
			return
		}
		if r.ContentLength <= 0 {
			fail(w, r, apperr.New(apperr.CodeInvalidRequest, "Content-Length is required and must be > 0 (chunked uploads are not supported)"))
			return
		}
		if r.ContentLength > s.Limits().MaxFileBytes {
			fail(w, r, apperr.New(apperr.CodeRequestTooLarge, "file exceeds files.max_file_bytes"))
			return
		}
		// 大文件需要比 server.read_timeout 更久的读取时间
		deadline := time.Now().Add(d.Config.Server.ReadTimeout + time.Duration(r.ContentLength>>20+1)*uploadReadPerMiB)
		if err := http.NewResponseController(w).SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			d.Logger.Debug("files: could not extend the read deadline", "err", err)
		}
		m, err := s.Put(owner(r), name, r.Header.Get("Content-Type"), r.ContentLength, ttl, r.Body)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, m)
	}))
	mux.Handle("GET /files", wrap(func(w http.ResponseWriter, r *http.Request) {
		list, used := d.Files.List(owner(r))
		lim := d.Files.Limits()
		writeJSON(w, http.StatusOK, map[string]any{"files": list, "used": used, "limits": map[string]any{
			"max_file_bytes": lim.MaxFileBytes, "max_files": lim.MaxFilesPerClient, "max_bytes": lim.MaxBytesPerClient,
			"default_ttl": lim.DefaultTTL.String(), "max_ttl": lim.MaxTTL.String(),
		}})
	}))
	mux.Handle("GET /files/{id}", wrap(func(w http.ResponseWriter, r *http.Request) {
		m, err := d.Files.Get(owner(r), r.PathValue("id"))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, m)
	}))
	mux.Handle("DELETE /files/{id}", wrap(func(w http.ResponseWriter, r *http.Request) {
		if err := d.Files.Delete(owner(r), r.PathValue("id")); err != nil {
			fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // 响应已开始，写失败只能放弃（客户端断开）
}
