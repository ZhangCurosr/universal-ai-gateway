# 14 供 AI 调用的接入方式（MCP / CLI / 客户端后端）

> 代码位置：`internal/mcp`（服务端 T32、客户端 T33）、`cmd/gateway/client_cmds.go`（子命令，已提供）、
> `internal/protocol/tool.go`（工具契约，已提供）。

网关接入的服务（大模型、搜索、文档解析……）可以用三种方式交给 AI 使用：

| 方式 | 适合 | 走的路径 |
|:--|:--|:--|
| **协议端点**（HTTP） | 把网关当作模型后端的 AI 客户端：Claude Code、Codex CLI、Cursor、各种 SDK | 各入站适配器声明的端点（`/v1/messages`、`/v1/chat/completions`、`/v1/responses`……） |
| **MCP 工具** | 支持 MCP 的 AI 智能体：把网关的服务当作「工具」调用（搜索、解析文档、问另一个模型） | `POST /mcp`（Streamable HTTP），或本地 `gateway mcp`（stdio） |
| **命令行** | 能执行 shell 命令的 AI 智能体与脚本 | `gateway tools`、`gateway call <tool>` |

三种方式**共用同一套**鉴权（客户端 Token）、白名单、限流、计费、选 Key 与重试：MCP 与命令行只是把工具调用转换为对协议端点的内部请求，交给内核处理。

```
AI 智能体 ──MCP (HTTP)──────────────▶ /mcp ──┐
AI 智能体 ──MCP (stdio)─▶ gateway mcp ─HTTP─▶ /mcp ──┤  mcp.Server ──内部请求──▶ core.Handler(入站适配器, 调用端点) ──▶ 上游
AI 智能体 ──shell──────▶ gateway call ─HTTP─▶ /mcp ──┘          ▲
                                                                  └── 工具由实现了 protocol.ToolSource 的入站适配器提供
```

## 1. 工具契约（`protocol.ToolSource`，已提供）

入站适配器**可选**实现 `ToolSource`，把本协议的操作暴露为工具：

```go
type ToolSource interface {
    Tools() []Tool
    ToolRequest(name string, args json.RawMessage) (*ToolRequest, error)  // 参数 → 对本协议一个调用端点的请求
    ToolResult(name string, status int, header http.Header, body []byte) (*ToolResult, error) // 端点响应 → 工具结果
}
```

| 编号 | 规则 |
|:--|:--|
| TS-01 | 工具名匹配 `^[A-Za-z0-9_.-]{1,64}$`，在整个注册表内唯一；`Description` 非空；`InputSchema` 是 `"type": "object"` 的 JSON Schema（REG-05，`Registry.Seal` 检查，已实现） |
| TS-02 | `Description` 写给 AI 看：用一两句话说明**什么时候用**、**返回什么**；`InputSchema` 的每个属性都写 `description`；能限制的都限制（`required`、`enum`、`minimum`、`additionalProperties: false`） |
| TS-03 | `ToolRequest` 只能生成本协议已声明的 `RouteCall` 端点的请求；**非流式**（工具结果需要完整响应）；服务名写在请求里（`Tool.Model` 非空时用它） |
| TS-04 | `ToolRequest` 参数不合法 → `CodeInvalidRequest`，消息说明哪个参数有问题 |
| TS-05 | `ToolResult` 对非 2xx 响应提取本协议错误格式中的消息，返回 `IsError = true` 的结果；只有响应完全无法解析时才返回 error |
| TS-06 | 结果优先给 AI 能直接使用的文本（模型回答、搜索结果摘要列表、解析得到的 Markdown）；结构化数据另放 `Structured`（必须是 JSON 对象） |
| TS-07 | 工具不得暴露上游凭据、网关 Token 或其他客户端的数据 |

当前提供的工具：

| 工具 | 适配器 | 说明 |
|:--|:--|:--|
| `ask_model` | `openai_responses`（T09C） | 参数 `model`、`input`、`instructions?`、`max_output_tokens?`；对任一可经 Responses 协议调用的服务提问，返回文本回答 |
| `tavily_search` | `tavily`（T30） | 参数 `query`、`search_depth?`、`max_results?`；调用 Tavily 搜索网页，返回标题 + URL + 摘要文本列表 |
| `tavily_extract` | `tavily`（T30） | 参数 `urls`；调用 Tavily 提取网页正文内容，返回正文文本列表 |
| `mineru_parse` 等 | `mineru`（T31） | 见 spec/08 §8.2；T31 实现时追加（同步封装，返回 Markdown） |

## 2. MCP 服务端（`internal/mcp.Server`，T32）

### 2.1 传输（Streamable HTTP）

| 编号 | 规则 |
|:--|:--|
| SRV-M01 | 端点 `/mcp`，挂在数据面，前面依次是 `IPRateLimit → Overload → Authenticate → RateLimit`（RTE-05）。客户端用 `Authorization: Bearer <网关 Token>` 鉴权（与协议端点相同的 Token） |
| SRV-M02 | 支持的协议版本 `SupportedVersions = ["2025-06-18", "2025-03-26"]`。`initialize` 时客户端请求的版本受支持则回同一版本，否则回 `SupportedVersions[0]` |
| SRV-M03 | 请求头 `MCP-Protocol-Version` 存在且不在 `SupportedVersions` 中 → 400（响应体为 JSON-RPC 错误）；不存在时按 `2025-03-26` 处理 |
| SRV-M04 | `POST`：请求体是**一条** JSON-RPC 消息（上限 4 MiB，超限 413）。批量（数组）→ `-32600`。通知与客户端发来的响应 → `202 Accepted`、无响应体。请求 → `200`、`Content-Type: application/json`、一条 JSON-RPC 响应。**不使用 SSE 回复**（规范允许服务端二选一） |
| SRV-M05 | `GET` 与 `DELETE` → `405 Method Not Allowed`（不提供服务端主动推送流；无状态服务端没有会话可删除）。**不签发** `Mcp-Session-Id`；客户端带了也忽略 |
| SRV-M06 | `Origin` 校验（防 DNS 重绑定）：请求带 `Origin` 头且其主机不是 `localhost`、`127.0.0.1`、`[::1]` → 403。非浏览器客户端不发送 `Origin`，不受影响 |

### 2.2 方法

| 编号 | 方法 | 行为 |
|:--|:--|:--|
| SRV-M07 | `initialize` | 结果 `{"protocolVersion", "capabilities": {"tools": {"listChanged": false}}, "serverInfo": {"name": "universal-ai-gateway", "version": Deps.Version}, "instructions": "<一句话：这些工具由网关代理，调用会计费>"}` |
| | `ping` | 结果 `{}` |
| | `notifications/*` | 不回复（202） |
| | `tools/list` | 结果 `{"tools": [...]}`（§2.3），忽略 `cursor`，不分页 |
| | `tools/call` | 参数 `{"name", "arguments"}`；结果 `{"content": [...], "structuredContent"?, "isError"}`（§2.4） |
| | 其他 | `-32601 method not found` |
| SRV-M08 | 中间件错误 | `mcp.WriteHTTPError`（已实现）：HTTP 状态码按错误码，响应体为 `id: null` 的 JSON-RPC 错误；401 带 `WWW-Authenticate: Bearer` |

### 2.3 工具列表（`listTools`）

| 编号 | 规则 |
|:--|:--|
| TL-01 | 来源是 `Registry.Tools()`，按名称升序 |
| TL-02 | 每个工具输出 `{"name", "title"?, "description", "inputSchema", "annotations": {"readOnlyHint": Tool.ReadOnly, "openWorldHint": true}}` |
| TL-03 | `Tool.Model` 非空：只有该服务已配置（`core.Route` 存在）且客户端白名单允许时才列出 |
| TL-04 | `Tool.Model` 为空（服务由参数指定）：可用服务 = `core.Models(principal)` 中上游协议等于工具所属协议、或注册了「工具协议 → 上游协议」转换器的项。为空则不列出；否则在 `description` 末尾追加 `"\n\nAvailable models: a, b, c"`（按名称升序） |

### 2.4 工具调用（`callTool`）

| 编号 | 规则 |
|:--|:--|
| TL-05 | 工具不存在（或按 TL-03/TL-04 对该客户端不可见）→ JSON-RPC 错误 `-32602 unknown tool`。这是 MCP 规范区分的「协议错误」 |
| TL-06 | `Tool.Model` 非空但客户端不允许 → `isError: true` 的结果（文本说明原因） |
| TL-07 | `ToolRequest` 返回错误 → `isError: true`，文本为错误消息（让 AI 自行修正参数重试） |
| TL-08 | 内部请求（`newInternalRequest`，已实现）：context 沿用外层请求（Principal、请求 ID、断开取消）；路径由路由模式与 `PathValues` 生成并 `SetPathValue`；**只**使用 `ToolRequest.Header`，不复制外层请求头。交给 `New` 时预先创建的 `core.Handler(入站适配器, 路由)`，写入有上限的 `bufferedWriter`（已实现）。内部请求**不再经过**中间件：外层 MCP 请求已经鉴权、占用并发名额、消耗了一个 RPM 令牌 |
| TL-09 | 端点响应超过 `mcp.max_result_bytes` → `isError: true`（`result exceeds mcp.max_result_bytes`）；计费照常（上游已经产生费用） |
| TL-10 | `ToolResult` 返回错误 → `isError: true`（`could not parse the service response`），记 `Warn` 日志（不含响应正文） |
| TL-11 | `content` 中每项按类型输出：`text` → `{"type":"text","text"}`；`image` → `{"type":"image","data","mimeType"}`；`resource` → `{"type":"resource","resource":{"uri","mimeType","text"}}` |

## 3. 命令行（`cmd/gateway/client_cmds.go`，已提供；依赖 T33 的 `mcp.Client`）

| 编号 | 规则 |
|:--|:--|
| CLI-01 | `Client.Post`：`POST <UAIG_URL>/mcp`，头 `Authorization: Bearer <UAIG_TOKEN>`、`Content-Type: application/json`、`Accept: application/json, text/event-stream`、`MCP-Protocol-Version: SupportedVersions[0]`；202 → nil；响应若是 `text/event-stream`（兼容其他服务端）取第一个 `data` 中的 JSON-RPC 响应；非 2xx → 错误（消息取 JSON-RPC 错误的 message） |
| CLI-02 | `ListTools`：`initialize` → `notifications/initialized` → `tools/list` |
| CLI-03 | `CallTool`：同上握手后 `tools/call` |
| CLI-04 | `gateway mcp` → `mcp.Bridge`：stdin 每行一条 JSON-RPC 消息，转发到 `/mcp`，非空响应作为一行写到 stdout；stdout **只能**出现 JSON-RPC 报文，诊断写 stderr；转发失败时对请求回一条该 id 的 `-32603` 错误，对通知忽略；stdin EOF 时退出码 0 |
| CLI-05 | 连接信息只来自环境变量 `UAIG_URL`（缺省 `http://127.0.0.1:8080`）与 `UAIG_TOKEN`（必填）；**不提供** `--token` 参数（会出现在进程列表与 shell 历史中）。Token 不得出现在任何输出中 |
| CLI-06 | `gateway tools [--json]`：每行 `名称 + 描述首行`；`--json` 输出完整列表 |
| CLI-07 | `gateway call <tool> [--args JSON\|-] [--json] [--timeout 10m]`：`--args -` 从 stdin 读参数；输出结果中的文本；`isError` 时退出码 1；参数不是 JSON 对象时退出码 2 |

## 4. 客户端配置示例（写进 `docs/DEPLOYMENT.md`，T15）

先用管理 API 为每个 AI 客户端创建一个客户端 Token，并用 `allowed_models` 限制它能用的服务。

### 4.1 作为模型后端

| 客户端 | 配置 |
|:--|:--|
| Claude Code | `ANTHROPIC_BASE_URL=https://gw.example.com`（不带 `/v1`）、`ANTHROPIC_AUTH_TOKEN=uai_live_...` |
| Codex CLI | `~/.codex/config.toml` 中定义 provider：`base_url = "https://gw.example.com/v1"`、`env_key = "UAIG_TOKEN"`、`wire_api = "responses"`（使用 `/v1/responses`，T09C） |
| OpenAI SDK / Cursor 等 | `base_url = https://gw.example.com/v1`，API Key = 客户端 Token |

### 4.2 作为 MCP 工具

远程（推荐，客户端机器不需要网关二进制）：

```bash
claude mcp add --transport http uaig https://gw.example.com/mcp --header "Authorization: Bearer uai_live_..."
```

本地 stdio（客户端只支持 stdio 时）：

```json
{ "mcpServers": { "uaig": {
    "command": "gateway", "args": ["mcp"],
    "env": { "UAIG_URL": "https://gw.example.com", "UAIG_TOKEN": "uai_live_..." } } } }
```

### 4.3 作为命令行

```bash
export UAIG_URL=https://gw.example.com UAIG_TOKEN=uai_live_...
gateway tools
gateway call ask_model --args '{"model":"deepseek-chat","input":"用一句话解释 CAP 定理"}'
```

## 5. 安全

- `/mcp` 与协议端点共用数据面监听地址；对公网开放时必须经过 HTTPS 反向代理（spec/12）。
- 工具调用是**有费用**的：给 AI 用的 Token 应设置额度（`quota_nano_usd`）与 RPM，并用 `allowed_models` 只开放需要的服务。
- MCP 规范的 OAuth 授权流程不在 M1 范围内；使用静态 Bearer Token（MCP 规范允许）。
