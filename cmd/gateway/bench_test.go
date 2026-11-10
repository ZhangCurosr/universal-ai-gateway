package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

var (
	benchReq2KiB       []byte
	chatStream100      []byte
	anthropicStream100 []byte
)

func init() {
	// 构造约 2 KiB 的 OpenAI Chat 请求体
	padding := strings.Repeat("Universal AI Gateway benchmark padding text. ", 44)
	benchReq2KiB, _ = json.Marshal(map[string]any{
		"model": "deepseek-test",
		"messages": []map[string]any{
			{"role": "user", "content": padding},
		},
	})

	// 构造 100 个事件的 OpenAI Chat 流式响应（99 delta + 1 usage + [DONE]）
	var csBuf bytes.Buffer
	for i := 0; i < 99; i++ {
		csBuf.WriteString(fmt.Sprintf(`data: {"id":"chatcmpl-bench","choices":[{"index":0,"delta":{"content":"w%d "},"finish_reason":null}]}`+"\n\n", i))
	}
	csBuf.WriteString(`data: {"id":"chatcmpl-bench","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":99}}` + "\n\n")
	csBuf.WriteString("data: [DONE]\n\n")
	chatStream100 = csBuf.Bytes()

	// 构造 100 个事件的 Anthropic 流式响应（1 start + 1 block_start + 95 deltas + 1 block_stop + 1 delta + 1 stop）
	var asBuf bytes.Buffer
	asBuf.WriteString("event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_bench","type":"message","role":"assistant","content":[],"model":"up-claude","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n")
	asBuf.WriteString("event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
	for i := 0; i < 95; i++ {
		asBuf.WriteString(fmt.Sprintf("event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a%d "}}`+"\n\n", i))
	}
	asBuf.WriteString("event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n")
	asBuf.WriteString("event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":96}}` + "\n\n")
	asBuf.WriteString("event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n")
	anthropicStream100 = asBuf.Bytes()
}

// newBenchUpstream 启动高性能假上游，预先序列化好所有响应，循环内零 sleep（spec/02 §10.1 BEN-02）。
func newBenchUpstream(t testing.TB) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		switch r.URL.Path {
		case "/v1/chat/completions":
			if bytes.Contains(body, []byte(`"stream":true`)) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(chatStream100)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-9","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
		case "/v1/messages":
			if bytes.Contains(body, []byte(`"stream":true`)) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(anthropicStream100)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"up-claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupBenchGateway(b *testing.B, upstreamURL string) (*gateway, string) {
	b.Helper()
	g := startGateway(b, upstreamURL, nil)
	adminURL := "http://" + g.admin + "/admin/api"

	var created struct {
		Token string `json:"token"`
	}
	code := doJSON(b, "POST", adminURL+"/clients", g.adminToken, map[string]any{
		"name":           "bench-client",
		"role":           "user",
		"quota_nano_usd": int64(1e15), // 巨额额度，确保基准不欠费
		"rpm_limit":      0,
	}, &created)
	if code != http.StatusCreated {
		b.Fatalf("create client failed: %d", code)
	}

	for _, pool := range []string{"chat", "anth", "oai"} {
		code := doJSON(b, "POST", adminURL+"/keys", g.adminToken, map[string]any{
			"pool":          pool,
			"name":          pool + "-k1",
			"secret":        "sk-bench-" + pool,
			"max_in_flight": 1000,
		}, nil)
		if code != http.StatusCreated {
			b.Fatalf("create key for %s failed: %d", pool, code)
		}
	}
	return g, created.Token
}

// BenchmarkE2EBufferedPassthrough 测量 OpenAI Chat 同协议非流式透传（约 2 KiB 请求体）。
func BenchmarkE2EBufferedPassthrough(b *testing.B) {
	b.ReportAllocs()
	up := newBenchUpstream(b)
	g, token := setupBenchGateway(b, up.URL)
	targetURL := "http://" + g.data + "/v1/chat/completions"

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest("POST", targetURL, bytes.NewReader(benchReq2KiB))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("request failed: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			b.Fatalf("status code = %d, want 200: %s", resp.StatusCode, body)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkE2EBaselineDirect 客户端直接请求 fakeUpstream 不走网关，用于计算网关自身开销。
func BenchmarkE2EBaselineDirect(b *testing.B) {
	b.ReportAllocs()
	up := newBenchUpstream(b)
	targetURL := up.URL + "/v1/chat/completions"

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest("POST", targetURL, bytes.NewReader(benchReq2KiB))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("direct request failed: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			b.Fatalf("status code = %d, want 200: %s", resp.StatusCode, body)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkE2EStreamPassthrough 测量 OpenAI Chat 同协议流式透传（100 个事件），并报告 TTFB。
func BenchmarkE2EStreamPassthrough(b *testing.B) {
	b.ReportAllocs()
	up := newBenchUpstream(b)
	g, token := setupBenchGateway(b, up.URL)
	targetURL := "http://" + g.data + "/v1/chat/completions"

	streamReq, _ := json.Marshal(map[string]any{
		"model":    "deepseek-test",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
		},
	}

	var totalTTFB time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest("POST", targetURL, bytes.NewReader(streamReq))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		t0 := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("stream request failed: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			b.Fatalf("status code = %d, want 200: %s", resp.StatusCode, body)
		}

		sc := bufio.NewScanner(resp.Body)
		var recordedTTFB bool
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data:") && !recordedTTFB {
				totalTTFB += time.Since(t0)
				recordedTTFB = true
			}
		}
		_ = resp.Body.Close()
	}
	b.ReportMetric(float64(totalTTFB.Nanoseconds())/float64(b.N), "ttfb-ns/op")
}

// BenchmarkE2EStreamTranslate 测量 OpenAI Chat 入站 -> Anthropic 上游流式转译（100 个事件），并报告 TTFB。
func BenchmarkE2EStreamTranslate(b *testing.B) {
	b.ReportAllocs()
	up := newBenchUpstream(b)
	g, token := setupBenchGateway(b, up.URL)
	targetURL := "http://" + g.data + "/v1/chat/completions"

	streamReq, _ := json.Marshal(map[string]any{
		"model":    "claude-test",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
		},
	}

	var totalTTFB time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest("POST", targetURL, bytes.NewReader(streamReq))
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		t0 := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("translate stream request failed: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			b.Fatalf("status code = %d, want 200: %s", resp.StatusCode, body)
		}

		sc := bufio.NewScanner(resp.Body)
		var recordedTTFB bool
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data:") && !recordedTTFB {
				totalTTFB += time.Since(t0)
				recordedTTFB = true
			}
		}
		_ = resp.Body.Close()
	}
	b.ReportMetric(float64(totalTTFB.Nanoseconds())/float64(b.N), "ttfb-ns/op")
}

// TestMemoryUnderLoad 测量高并发流式请求下的内存与协程开销（spec/02 §10.1 BEN-06，spec/12 §4）。
func TestMemoryUnderLoad(t *testing.T) {
	if os.Getenv("UAIG_MEMTEST") != "1" {
		t.Skip("set UAIG_MEMTEST=1 to run memory load test")
	}

	runLoadTest := func(t *testing.T, name string, concurrency int, mutate func(*config.Config)) {
		t.Run(name, func(t *testing.T) {
			// 慢速假上游：每路 10ms 发送一个事件，共 500 个事件（持续 ~5s）
			slowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, ok := w.(http.Flusher)
				if !ok {
					http.Error(w, "streaming unsupported", http.StatusInternalServerError)
					return
				}
				for i := 0; i < 500; i++ {
					_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"c%d \"}}]}\n\n", i)
					flusher.Flush()
					time.Sleep(10 * time.Millisecond)
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				flusher.Flush()
			}))
			defer slowUpstream.Close()

			g := startGateway(t, slowUpstream.URL, mutate)
			adminURL := "http://" + g.admin + "/admin/api"

			var created struct {
				Token string `json:"token"`
			}
			code := doJSON(t, "POST", adminURL+"/clients", g.adminToken, map[string]any{
				"name":           "mem-client",
				"role":           "user",
				"quota_nano_usd": int64(1e15),
				"rpm_limit":      0,
			}, &created)
			if code != http.StatusCreated {
				t.Fatalf("create client failed: %d", code)
			}
			code = doJSON(t, "POST", adminURL+"/keys", g.adminToken, map[string]any{
				"pool":          "chat",
				"name":          "chat-k1",
				"secret":        "sk-mem-chat",
				"max_in_flight": 1000,
			}, nil)
			if code != http.StatusCreated {
				t.Fatalf("create key failed: %d", code)
			}

			startGoroutines := runtime.NumGoroutine()
			var maxHeapInuse uint64
			var maxSys uint64
			var maxGoroutines int

			stopSample := make(chan struct{})
			var sampleWg sync.WaitGroup
			sampleWg.Add(1)
			go func() {
				defer sampleWg.Done()
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopSample:
						return
					case <-ticker.C:
						var m runtime.MemStats
						runtime.ReadMemStats(&m)
						ng := runtime.NumGoroutine()
						if m.HeapInuse > maxHeapInuse {
							maxHeapInuse = m.HeapInuse
						}
						if m.Sys > maxSys {
							maxSys = m.Sys
						}
						if ng > maxGoroutines {
							maxGoroutines = ng
						}
					}
				}
			}()

			client := &http.Client{
				Transport: &http.Transport{
					MaxIdleConns:        concurrency,
					MaxIdleConnsPerHost: concurrency,
				},
			}

			targetURL := "http://" + g.data + "/v1/chat/completions"
			streamBody, _ := json.Marshal(map[string]any{
				"model":    "deepseek-test",
				"stream":   true,
				"messages": []map[string]any{{"role": "user", "content": "hi"}},
			})

			var reqWg sync.WaitGroup
			statusCodes := make([]int, concurrency)
			errBodies := make([]string, concurrency)
			for i := 0; i < concurrency; i++ {
				reqWg.Add(1)
				go func(idx int) {
					defer reqWg.Done()
					req, err := http.NewRequest("POST", targetURL, bytes.NewReader(streamBody))
					if err != nil {
						return
					}
					req.Header.Set("Authorization", "Bearer "+created.Token)
					req.Header.Set("Content-Type", "application/json")

					resp, err := client.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					statusCodes[idx] = resp.StatusCode
					b, _ := io.ReadAll(resp.Body)
					if resp.StatusCode != http.StatusOK {
						errBodies[idx] = string(b)
					}
				}(i)
			}

			reqWg.Wait()
			close(stopSample)
			sampleWg.Wait()

			for idx, sc := range statusCodes {
				if sc != http.StatusOK {
					t.Fatalf("request %d returned %d, want 200: %s", idx, sc, errBodies[idx])
				}
			}

			// 关闭客户端空闲连接，使读写协程释放
			client.CloseIdleConnections()
			slowUpstream.CloseClientConnections()

			// 检查协程泄漏：等待 2 秒后回到开始前 + 10 以内
			time.Sleep(2 * time.Second)
			endGoroutines := runtime.NumGoroutine()
			if endGoroutines > startGoroutines+10 {
				buf := make([]byte, 64<<10)
				n := runtime.Stack(buf, true)
				t.Logf("Stack dump of %d goroutines:\n%s", endGoroutines, buf[:n])
				t.Errorf("goroutine leak: started with %d, peak %d, ended with %d", startGoroutines, maxGoroutines, endGoroutines)
			}

			t.Logf("[%s] Peak HeapInuse: %d bytes (%.2f MiB), Peak Sys: %d bytes (%.2f MiB), Peak Goroutines: %d (Start: %d, End: %d)",
				name, maxHeapInuse, float64(maxHeapInuse)/(1024*1024), maxSys, float64(maxSys)/(1024*1024), maxGoroutines, startGoroutines, endGoroutines)
		})
	}

	// 1. 默认配置档（>= 4 GiB 机器，256 路并发）
	runLoadTest(t, "Default_256Inflight", 256, nil)

	// 2. 最小内存档（spec/12 §4 1 GiB 机器：64 inflight / 128 MiB 缓冲）
	runLoadTest(t, "Minimal_64Inflight", 64, func(c *config.Config) {
		c.Limits.MaxInFlightRequests = 64
		c.Limits.MaxBufferedBodyBytes = 128 << 20
		c.Limits.MaxRequestBodyBytes = 16 << 20
	})
}
