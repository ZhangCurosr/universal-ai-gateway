package mineru

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/pricing"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"
)

// CORE-12：下载失败仍报告已完成任务的实际费用；提交失败不产生计费结果。
func TestPaidFailuresRetainUsage(t *testing.T) {
	for _, tt := range []struct {
		name, op        string
		submitStatus    int
		breakSubmitBody bool
		wantPages       int64
		wantSource      protocol.UsageSource
	}{
		{name: "download failed", op: OpParse, submitStatus: 200, wantPages: 7, wantSource: protocol.UsageUpstream},
		{name: "submit body interrupted", op: OpParse, submitStatus: 200, breakSubmitBody: true, wantPages: 3, wantSource: protocol.UsageEstimated},
		{name: "async submit body interrupted", op: OpTaskCreate, submitStatus: 200, breakSubmitBody: true, wantPages: 3, wantSource: protocol.UsageEstimated},
		{name: "submission rejected", op: OpParse, submitStatus: 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					w.WriteHeader(tt.submitStatus)
					if tt.breakSubmitBody {
						w.(http.Flusher).Flush()
						panic(http.ErrAbortHandler)
					}
					_, _ = io.WriteString(w, `{"code":0,"data":{"task_id":"paid-task"}}`)
				case r.URL.Path == "/api/v4/extract/task/paid-task":
					_, _ = io.WriteString(w, `{"code":0,"data":{"state":"done","full_zip_url":"`+srv.URL+`/files/r.zip","extract_progress":{"total_pages":7}}}`)
				default:
					w.WriteHeader(503)
				}
			}))
			defer srv.Close()
			resp, err := fastPoll.Do(context.Background(), protocol.Upstream{BaseURL: srv.URL, Client: srv.Client()},
				&protocol.Call{Op: tt.op, Body: urlBody, State: pricing.Usage{UnitPages: 3}})
			var ae *protocol.AttemptError
			if !errors.As(err, &ae) {
				t.Fatalf("Do() error = %v, want AttemptError", err)
			}
			if tt.submitStatus != 200 {
				if resp != nil {
					t.Fatal("rejected submission returned a billable response")
				}
				return
			}
			if resp == nil || resp.Meter == nil || len(resp.Body) != 0 {
				t.Fatalf("paid failure response = %+v, want meter without partial output", resp)
			}
			u, src := resp.Meter.Usage()
			if u[UnitPages] != tt.wantPages || u[UnitRequests] != 1 || src != tt.wantSource {
				t.Fatalf("paid failure usage = %v (%v), want pages=%d requests=1 source=%v", u, src, tt.wantPages, tt.wantSource)
			}
		})
	}
}
