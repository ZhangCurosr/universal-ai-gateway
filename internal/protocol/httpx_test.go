package protocol

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/sse"
)

func TestSend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, "fine")
		case "/bad":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Set-Cookie", "x=y")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"bad"}`+strings.Repeat(" ", MaxErrorBodyBytes))
		case "/limited":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	up := Upstream{Client: srv.Client()}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/ok", nil)
	resp, err := Send(up, req)
	if err != nil {
		t.Fatalf("Send(ok) error = %v", err)
	}
	if b, _ := ReadBody(up, resp); string(b) != "fine" {
		t.Fatalf("body = %q", b)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/bad", nil)
	_, err = Send(up, req)
	var ae *AttemptError
	if !errors.As(err, &ae) || ae.Outcome.Kind != OutcomeClientError || ae.Outcome.Status != 400 {
		t.Fatalf("Send(bad) error = %v", err)
	}
	if len(ae.Body) != MaxErrorBodyBytes || ae.Header.Get("Content-Type") != "application/json" || ae.Header.Get("Set-Cookie") != "" {
		t.Fatalf("error body len=%d header=%v", len(ae.Body), ae.Header)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/limited", nil)
	if _, err = Send(up, req); !errors.As(err, &ae) || ae.Outcome.Kind != OutcomeRateLimited || ae.Outcome.RetryAfter.Seconds() != 7 {
		t.Fatalf("Send(limited) error = %v", err)
	}

	req, _ = http.NewRequest(http.MethodGet, "http://127.0.0.1:1/unreachable", nil)
	if _, err = Send(up, req); !errors.As(err, &ae) || ae.Outcome.Kind != OutcomeNetworkError {
		t.Fatalf("Send(unreachable) error = %v", err)
	}
}

func TestReadBodyLimit(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("123456"))}
	_, err := ReadBody(Upstream{MaxBufferedBytes: 5}, resp)
	var ae *AttemptError
	if !errors.As(err, &ae) || !errors.Is(err, ErrResponseTooLarge) || ae.Outcome.Kind != OutcomeLocalError {
		t.Fatalf("ReadBody() error = %v, want too large", err)
	}
}

func TestJoinURL(t *testing.T) {
	for _, tt := range [][3]string{
		{"https://a.example/", "/v1/x", "https://a.example/v1/x"},
		{"https://a.example/base", "v1/x?y=1", "https://a.example/base/v1/x?y=1"},
	} {
		if got := JoinURL(tt[0], tt[1]); got != tt[2] {
			t.Errorf("JoinURL(%q, %q) = %q, want %q", tt[0], tt[1], got, tt[2])
		}
	}
}

func TestNewSSEStream(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: a\n\ndata: skip\n\ndata: b\n\ndata: boom\n\n"))
	var seen []string
	s := NewSSEStream(body, func(ev sse.Event) (bool, error) {
		seen = append(seen, string(ev.Data))
		if string(ev.Data) == "boom" {
			return false, errors.New("boom")
		}
		return string(ev.Data) != "skip", nil
	})
	defer s.Close()
	var got []string
	for {
		ev, err := s.Next()
		if err != nil {
			if err.Error() != "boom" {
				t.Fatalf("Next() error = %v, want boom", err)
			}
			break
		}
		got = append(got, string(ev.Data))
	}
	if strings.Join(got, ",") != "a,b" || len(seen) != 4 {
		t.Fatalf("forwarded %v, inspected %v", got, seen)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
}
