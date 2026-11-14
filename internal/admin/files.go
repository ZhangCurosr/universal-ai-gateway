package admin

import (
	"net/http"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/files"
)

// FilesView 是 GET /admin/api/files 的响应（FS-50，T69）。
type FilesView struct {
	Enabled bool `json:"enabled"`
	*files.Stats
	Limits *FilesLimitsView `json:"limits,omitempty"`
}

// FilesLimitsView 是文件中转的上限。
type FilesLimitsView struct {
	MaxFileBytes      int64  `json:"max_file_bytes"`
	MaxFilesPerClient int    `json:"max_files_per_client"`
	MaxBytesPerClient int64  `json:"max_bytes_per_client"`
	MaxTotalBytes     int64  `json:"max_total_bytes"`
	DefaultTTL        string `json:"default_ttl"`
	MaxTTL            string `json:"max_ttl"`
}

var errFilesDisabled = apperr.New(apperr.CodeNotFound, "file relay is not enabled (files.enabled)")

// handleFiles 实现 FS-50：没有开启文件中转时只返回 {"enabled": false}。
func (s *server) handleFiles(w http.ResponseWriter, _ *http.Request) {
	fs := s.d.Files
	if fs == nil {
		writeJSON(w, http.StatusOK, FilesView{})
		return
	}
	st := fs.Stats()
	lim := fs.Limits()
	writeJSON(w, http.StatusOK, FilesView{Enabled: true, Stats: &st, Limits: &FilesLimitsView{
		MaxFileBytes: lim.MaxFileBytes, MaxFilesPerClient: lim.MaxFilesPerClient, MaxBytesPerClient: lim.MaxBytesPerClient,
		MaxTotalBytes: lim.MaxTotalBytes, DefaultTTL: lim.DefaultTTL.String(), MaxTTL: lim.MaxTTL.String(),
	}})
}

// handleDeleteFile 实现 FS-50：管理员删除任意文件。
func (s *server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	if s.d.Files == nil {
		writeError(w, errFilesDisabled)
		return
	}
	if err := s.d.Files.AdminDelete(r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
