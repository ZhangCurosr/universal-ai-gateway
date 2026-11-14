package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

// 管理面 MCP（spec/14 §6，任务卡 T58）。
//
// 挂在管理面的 /admin/mcp（admin.Deps.MCP），先经过管理面的鉴权（管理员 Token、Host 检查、IP 锁定、
// 请求体上限）。每个工具都转换成一个进程内的管理 API 请求，交给完整的管理面处理器，因此鉴权、校验、
// 错误格式与直接调用管理 API 完全相同（AM-04）。
//
// 不提供新建客户端、轮换 Token、添加 Key 的工具（AM-03）：它们会把凭据交给 AI。
// internal/admin 与 internal/mcp 同层不能互相导入，所以本文件在 cmd/gateway 中。

const adminMCPMaxResult = 1 << 20

type adminTool struct {
	name, title, desc string
	schema            string
	readOnly          bool
	destructive       bool
	// request 把参数转换为管理 API 请求（方法、路径含查询串、请求体）；参数不合法时返回错误。
	request func(args json.RawMessage) (method, path string, body []byte, err error)
}

// adminMCP 是管理面 MCP 的 http.Handler。admin 在管理面处理器创建后设置。
type adminMCP struct {
	admin  http.Handler
	logger *slog.Logger
	tools  []adminTool
}

func newAdminMCP(logger *slog.Logger) *adminMCP {
	return &adminMCP{logger: logger, tools: adminTools()}
}

const noArgsSchema = `{"type":"object","properties":{},"additionalProperties":false}`

func noArgs(method, path string) func(json.RawMessage) (string, string, []byte, error) {
	return func(json.RawMessage) (string, string, []byte, error) { return method, path, nil, nil }
}

// decodeArgs 严格解析参数：未知字段与类型错误都报错。
func decodeAdminArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// adminTools 实现 AM-02。
func adminTools() []adminTool {
	return []adminTool{
		{name: "gateway_admin_status", title: "Gateway status", readOnly: true, schema: noArgsSchema,
			desc:    "Version, uptime and health of the gateway (ledger queue, dropped audit records).",
			request: noArgs(http.MethodGet, "/admin/api/health")},
		{name: "gateway_admin_list_clients", title: "List clients", readOnly: true, schema: noArgsSchema,
			desc:    "All gateway clients with role, quota, spend, RPM limit, allowed services and whether they are disabled. Tokens are never shown.",
			request: noArgs(http.MethodGet, "/admin/api/clients")},
		{name: "gateway_admin_list_keys", title: "List upstream keys", readOnly: true, schema: noArgsSchema,
			desc:    "All upstream keys per pool with masked hint, state (ready, disabled, quarantined, orphaned, expired, exhausted), state_reason, expiry time (expires_at, RFC 3339), source label, quota used / limit / next reset (pools with a quota), cooldown, breaker and in-flight load. Secrets are never shown.",
			request: noArgs(http.MethodGet, "/admin/api/keys")},
		{name: "gateway_admin_usage", title: "Query usage", readOnly: true,
			desc:   "Usage and cost per client and service for a time range (default: last 24 hours). Times are RFC 3339 in UTC.",
			schema: `{"type":"object","properties":{"client":{"type":"string","description":"Client name or id; empty for all clients"},"from":{"type":"string","description":"Start, RFC 3339, e.g. 2026-11-11T00:00:00Z"},"to":{"type":"string","description":"End, RFC 3339"}},"additionalProperties":false}`,
			request: func(args json.RawMessage) (string, string, []byte, error) {
				var a struct{ Client, From, To string }
				if err := decodeAdminArgs(args, &a); err != nil {
					return "", "", nil, err
				}
				q := url.Values{}
				for k, v := range map[string]string{"client": a.Client, "from": a.From, "to": a.To} {
					if v != "" {
						q.Set(k, v)
					}
				}
				path := "/admin/api/usage"
				if len(q) > 0 {
					path += "?" + q.Encode()
				}
				return http.MethodGet, path, nil, nil
			}},
		{name: "gateway_admin_config", title: "Configured pools and services", readOnly: true, schema: noArgsSchema,
			desc:    "Pools (name, protocol) and services (name, pool) of the running configuration.",
			request: noArgs(http.MethodGet, "/admin/api/config")},
		{name: "gateway_admin_tools", title: "AI tools", readOnly: true, schema: noArgsSchema,
			desc:    "Every tool the gateway offers to AI clients (built-in and declared in the config), with service, read-only flag and summary, plus the MCP tool list mode.",
			request: noArgs(http.MethodGet, "/admin/api/tools")},
		{name: "gateway_admin_alerts", title: "Key pool alerts", readOnly: true, schema: noArgsSchema,
			desc:    "Pool alerts firing now (ADM-26): low_keys (fewer available keys than the pool's minimum; critical when none are left), low_quota (little quota left this period) and expiring (keys expiring soon), each with value, threshold, start time and message. An empty list means no pool needs attention.",
			request: noArgs(http.MethodGet, "/admin/api/alerts")},
		{name: "gateway_admin_pools", title: "Key pool capacity", readOnly: true, schema: noArgsSchema,
			desc:    "Per pool (ADM-30): keys by state (available includes keys briefly cooling down), keys expiring within 7 days, billed requests and attempt failure rate over the last 24 hours, and for pools with a quota: used and left this period, burn per hour (last 24 hours), the next reset, and either runs_out_at or lasts_until_reset. Use it to answer whether a pool will last until its quota resets.",
			request: noArgs(http.MethodGet, "/admin/api/pools")},
		{name: "gateway_admin_key_stats", title: "Per-key usage and failures", readOnly: true,
			desc:   "Per upstream key (ADM-29): billed requests, usage per unit and cost for a time range (default: last 24 hours, at most 31 days), plus attempts, failures by kind (rate_limited, auth_failed, permission_denied, overloaded, server_error, timeout, network_error), failure rate and the last failure over the last 24 hours. Secrets are never shown.",
			schema: `{"type":"object","properties":{"pool":{"type":"string","description":"Only keys of this pool; empty for all"},"from":{"type":"string","description":"Start, RFC 3339"},"to":{"type":"string","description":"End, RFC 3339"}},"additionalProperties":false}`,
			request: func(args json.RawMessage) (string, string, []byte, error) {
				var a struct{ Pool, From, To string }
				if err := decodeAdminArgs(args, &a); err != nil {
					return "", "", nil, err
				}
				q := url.Values{}
				for k, v := range map[string]string{"pool": a.Pool, "from": a.From, "to": a.To} {
					if v != "" {
						q.Set(k, v)
					}
				}
				path := "/admin/api/keys/stats"
				if len(q) > 0 {
					path += "?" + q.Encode()
				}
				return http.MethodGet, path, nil, nil
			}},
		{name: "gateway_admin_reload_records", title: "Reload history", readOnly: true, schema: noArgsSchema,
			desc:    "The last 20 config reloads: time, mode (hot or restart), result (applied, failed, rolled_back, restarting) and error.",
			request: noArgs(http.MethodGet, "/admin/api/reload")},
		{name: "gateway_admin_update_client", title: "Update a client", destructive: true,
			desc:   "Change a client's quota, RPM limit, allowed services, name or disabled flag. Takes effect immediately. Quota is in nano-USD (1 USD = 1000000000).",
			schema: `{"type":"object","properties":{"id":{"type":"integer","minimum":1},"name":{"type":"string"},"quota_unlimited":{"type":"boolean"},"quota_nano_usd":{"type":"integer","minimum":0},"rpm_limit":{"type":"integer","minimum":0},"allowed_models":{"type":"array","items":{"type":"string"},"description":"Service names; empty array = all services"},"disabled":{"type":"boolean"}},"required":["id"],"additionalProperties":false}`,
			request: func(args json.RawMessage) (string, string, []byte, error) {
				var a struct {
					ID             int64     `json:"id"`
					Name           *string   `json:"name"`
					QuotaUnlimited *bool     `json:"quota_unlimited"`
					QuotaNanoUSD   *int64    `json:"quota_nano_usd"`
					RPMLimit       *int      `json:"rpm_limit"`
					AllowedModels  *[]string `json:"allowed_models"`
					Disabled       *bool     `json:"disabled"`
				}
				if err := decodeAdminArgs(args, &a); err != nil {
					return "", "", nil, err
				}
				if a.ID < 1 {
					return "", "", nil, errors.New("invalid arguments: id is required")
				}
				body, err := json.Marshal(map[string]any{"name": a.Name, "quota_unlimited": a.QuotaUnlimited, "quota_nano_usd": a.QuotaNanoUSD,
					"rpm_limit": a.RPMLimit, "allowed_models": a.AllowedModels, "disabled": a.Disabled})
				if err != nil {
					return "", "", nil, err
				}
				return http.MethodPatch, "/admin/api/clients/" + strconv.FormatInt(a.ID, 10), dropNulls(body), nil
			}},
		{name: "gateway_admin_set_key_state", title: "Enable or disable an upstream key", destructive: true,
			desc:   "Set an upstream key to ready (back in rotation, clears quarantine) or disabled (taken out of rotation). Use it for keys that keep failing with 403 or are exhausted.",
			schema: `{"type":"object","properties":{"id":{"type":"integer","minimum":1},"state":{"type":"string","enum":["ready","disabled"]}},"required":["id","state"],"additionalProperties":false}`,
			request: func(args json.RawMessage) (string, string, []byte, error) {
				var a struct {
					ID    int64  `json:"id"`
					State string `json:"state"`
				}
				if err := decodeAdminArgs(args, &a); err != nil {
					return "", "", nil, err
				}
				if a.ID < 1 || (a.State != "ready" && a.State != "disabled") {
					return "", "", nil, errors.New("invalid arguments: id is required and state must be ready or disabled")
				}
				body, _ := json.Marshal(map[string]string{"state": a.State}) // 固定结构，必然可以序列化
				return http.MethodPatch, "/admin/api/keys/" + strconv.FormatInt(a.ID, 10), body, nil
			}},
		{name: "gateway_admin_reload", title: "Reload the config file",
			desc:   "Re-read the gateway config file. Run with dry_run first to see what would change (hot reload or in-process restart). An in-process restart returns immediately; check gateway_admin_reload_records for the result.",
			schema: `{"type":"object","properties":{"dry_run":{"type":"boolean","description":"Only report what would change"}},"additionalProperties":false}`,
			request: func(args json.RawMessage) (string, string, []byte, error) {
				var a struct {
					DryRun bool `json:"dry_run"`
				}
				if err := decodeAdminArgs(args, &a); err != nil {
					return "", "", nil, err
				}
				body, _ := json.Marshal(map[string]bool{"dry_run": a.DryRun}) // 同上
				return http.MethodPost, "/admin/api/reload", body, nil
			}},
	}
}

// dropNulls 去掉 JSON 对象中值为 null 的字段（PATCH 只发送提供了的字段）。
func dropNulls(obj []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(obj, &m) != nil {
		return obj
	}
	for k, v := range m {
		if string(v) == "null" {
			delete(m, k)
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return obj
	}
	return out
}

const adminMCPInstructions = "These tools manage the universal-ai-gateway itself. Write tools take effect immediately. " +
	"Creating clients, rotating tokens and adding upstream keys are not available here because they would expose credentials: " +
	"use the admin web UI, and `gateway keys import` to add keys from a local file."

// ServeHTTP 实现 AM-01（传输规则同 SRV-M02 ~ SRV-M07）。
func (s *adminMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !localOrigin(origin) {
		writeRPC(w, http.StatusForbidden, mcp.ErrorResponse(nil, mcp.CodeInvalidRequest, "forbidden origin"))
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcp.SupportedVersions, v) {
		writeRPC(w, http.StatusBadRequest, mcp.ErrorResponse(nil, mcp.CodeInvalidRequest, "unsupported protocol version"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mcp.MaxMessageBytes+1))
	if err != nil || int64(len(body)) > mcp.MaxMessageBytes {
		writeRPC(w, http.StatusRequestEntityTooLarge, mcp.ErrorResponse(nil, mcp.CodeInvalidRequest, "request body too large or unreadable"))
		return
	}
	req, err := mcp.DecodeRequest(body)
	if err != nil {
		var rpcErr *mcp.Error
		if errors.As(err, &rpcErr) {
			writeRPC(w, http.StatusOK, mcp.ErrorResponse(nil, rpcErr.Code, rpcErr.Message))
			return
		}
		w.WriteHeader(http.StatusAccepted) // 客户端发来的响应
		return
	}
	if req.IsNotification() || strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, s.dispatch(r, req))
}

func localOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func writeRPC(w http.ResponseWriter, status int, resp *mcp.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp) // 写客户端失败无可处理
}

func (s *adminMCP) dispatch(r *http.Request, req *mcp.Request) *mcp.Response {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p) // 缺省或格式不对时按最新版本协商
		v := mcp.SupportedVersions[0]
		if slices.Contains(mcp.SupportedVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return mcp.ResultResponse(req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "universal-ai-gateway-admin", "version": version},
			"instructions":    adminMCPInstructions,
		})
	case "ping":
		return mcp.ResultResponse(req.ID, map[string]any{})
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			ann := map[string]any{"readOnlyHint": t.readOnly, "openWorldHint": false}
			if !t.readOnly {
				ann["destructiveHint"] = t.destructive
			}
			list = append(list, map[string]any{"name": t.name, "title": t.title, "description": t.desc,
				"inputSchema": json.RawMessage(t.schema), "annotations": ann})
		}
		return mcp.ResultResponse(req.ID, map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return mcp.ErrorResponse(req.ID, mcp.CodeInvalidParams, "invalid params")
		}
		for _, t := range s.tools {
			if t.name == p.Name {
				return mcp.ResultResponse(req.ID, s.call(r, t, p.Arguments))
			}
		}
		return mcp.ErrorResponse(req.ID, mcp.CodeInvalidParams, "unknown tool")
	default:
		return mcp.ErrorResponse(req.ID, mcp.CodeMethodNotFound, "method not found")
	}
}

func toolText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// call 实现 AM-04、AM-05。
func (s *adminMCP) call(outer *http.Request, t adminTool, args json.RawMessage) map[string]any {
	method, path, body, err := t.request(args)
	if err != nil {
		return toolText(err.Error(), true)
	}
	in, err := http.NewRequestWithContext(outer.Context(), method, path, bytes.NewReader(body))
	if err != nil {
		return toolText("internal error", true)
	}
	in.Header.Set("Authorization", outer.Header.Get("Authorization"))
	if body != nil {
		in.Header.Set("Content-Type", "application/json")
	}
	in.RemoteAddr, in.Host = outer.RemoteAddr, outer.Host
	rec := &limitedRecorder{header: http.Header{}, limit: adminMCPMaxResult}
	s.admin.ServeHTTP(rec, in)
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	s.logger.Info("admin mcp call", "tool", t.name, "status", status)
	if rec.overflow {
		return toolText("result exceeds 1 MiB", true)
	}
	out := rec.body.Bytes()
	if status < 200 || status > 299 {
		return toolText(fmt.Sprintf("HTTP %d: %s", status, strings.TrimSpace(string(out))), true)
	}
	res := toolText(strings.TrimSpace(string(out)), false)
	if t := bytes.TrimSpace(out); len(t) > 0 && t[0] == '{' && json.Valid(t) {
		res["structuredContent"] = json.RawMessage(t)
	}
	return res
}

// limitedRecorder 是有上限的 http.ResponseWriter，收集进程内管理 API 请求的响应。
type limitedRecorder struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int
	overflow bool
}

func (r *limitedRecorder) Header() http.Header { return r.header }
func (r *limitedRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *limitedRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.body.Len()+len(p) > r.limit {
		r.overflow = true
		return len(p), nil
	}
	return r.body.Write(p)
}
