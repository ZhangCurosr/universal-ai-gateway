package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

func (s *server) handleQueryUsage(w http.ResponseWriter, r *http.Request) {
	now := s.d.Now().UTC()
	var from, to time.Time

	if toStr := r.URL.Query().Get("to"); toStr != "" {
		var err error
		to, err = time.Parse(time.RFC3339, toStr)
		if err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid to parameter: RFC 3339 required"))
			return
		}
	} else {
		to = now
	}

	if fromStr := r.URL.Query().Get("from"); fromStr != "" {
		var err error
		from, err = time.Parse(time.RFC3339, fromStr)
		if err != nil {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid from parameter: RFC 3339 required"))
			return
		}
	} else {
		from = to.Add(-24 * time.Hour)
	}

	// ADM-17: from >= to 返回 400
	if !from.Before(to) {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "from must be before to"))
		return
	}

	// to - from 不得超过 92 天
	if to.Sub(from) > 92*24*time.Hour {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "time range cannot exceed 92 days"))
		return
	}

	var clientID int64
	if cidStr := r.URL.Query().Get("client_id"); cidStr != "" {
		var err error
		clientID, err = strconv.ParseInt(cidStr, 10, 64)
		if err != nil || clientID < 0 {
			writeError(w, apperr.New(apperr.CodeInvalidRequest, "invalid client_id"))
			return
		}
	}

	summaries, err := s.d.Store.QueryUsage(r.Context(), store.UsageQuery{
		ClientID: clientID,
		From:     from,
		To:       to,
	})
	if err != nil {
		writeError(w, err)
		return
	}

	items := make([]UsageItem, 0, len(summaries))
	for _, sm := range summaries {
		u := sm.Usage
		if u == nil {
			u = map[string]int64{}
		}
		items = append(items, UsageItem{
			ClientID:    sm.ClientID,
			Model:       sm.Model,
			Requests:    sm.Requests,
			Usage:       u,
			CostNanoUSD: sm.CostNanoUSD,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
	})
}
