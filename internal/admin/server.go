package admin

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/crypto"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/ledger"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/scheduler"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// Deps 是管理面的依赖。
type Deps struct {
	Config    *config.Config
	Store     store.Store
	Keyring   *crypto.Keyring
	Index     *auth.Index
	Reloader  *auth.Reloader
	Ledger    *ledger.Memory
	Scheduler *scheduler.Scheduler
	Logger    *slog.Logger
	Version   string
	StartedAt time.Time
	Now       func() time.Time // 为 nil 时用 time.Now
}

// NewServer 返回管理面 http.Handler（spec/09 §4）。
func NewServer(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	mux := http.NewServeMux()
	// TODO(T12): 注册 spec/09 §4.1 的全部端点，并套用 ADM-01 ~ ADM-04 的鉴权中间件。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, apperr.NotImplemented("T12"))
	})
	return mux
}
