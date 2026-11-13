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
| `mineru_parse` | `mineru`（T31） | 参数 `url`；调用 MinerU 解析文档（PDF/DOCX 等），返回 Markdown 文本（同步封装） |

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
| TL-12 | `fillPath`：`{name...}` 通配段的值是**已转义的路径**（与 `URL.EscapedPath()` 同一形式）：必须能被 `url.PathUnescape` 解码，且不含 `?`、`#`、空白与控制字符，否则 `-32602`；合法时原样写入，允许为空；内部请求中该名字的 `PathValue` 为解码后的值。`{name}` 的值仍整体 `url.PathEscape` 且不允许为空 |

### 2.5 工具目录（任务卡 T54）

工具数量多时，把全部工具的描述与 Schema 放进 `tools/list` 会长期占用 AI 的上下文。目录模式下只列出三个固定的目录工具（搜索、查看、调用）与置顶工具，AI 按需查找；常驻上下文与工具数量无关。目录在 `internal/mcp` 实现，与协议无关，可见性与 TL-03 / TL-04 相同，目录工具本身不访问上游、不计费。

| 编号 | 规则 |
|:--|:--|
| CAT-01 | 目录工具三个：`gateway_search_tools`、`gateway_describe_tool`、`gateway_call_tool`，由 `internal/mcp` 提供，不进入注册表。注册表中出现同名工具 → 网关拒绝启动 |
| CAT-02 | 生效模式按请求的客户端计算：`direct` → 直接；`catalog` → 目录；`auto` → 该客户端按 TL-03 / TL-04 可见的真实工具数 `<= auto_threshold` 时直接，否则目录。直接模式的 `tools/list` 与现状相同（TL-01 ~ TL-04），不出现目录工具 |
| CAT-03 | 目录模式的 `tools/list` = 三个目录工具 + `pinned_tools` 中对该客户端可见的工具（按 TL-02 输出），按名称升序。`initialize` 的 `instructions` 在目录模式下追加一句：用 `gateway_search_tools` 查找可用工具，用 `gateway_describe_tool` 查看参数，用 `gateway_call_tool` 调用 |
| CAT-04 | `gateway_search_tools`，参数 `{query?: string, limit?: integer 1–100（缺省 20）, offset?: integer >= 0（缺省 0）}`，只读。`query` 按空白拆成词，不区分大小写；工具的名称、标题、描述同时包含全部词即为匹配；`query` 为空则全部匹配。范围是该客户端可见的真实工具（不含目录工具），按名称升序后分页。结果文本每个工具一行 `名称 — 描述首行`（首行超过 160 个字符时截断）；`Structured` = `{"tools": [{"name", "title"?, "summary", "read_only"}], "total": 匹配总数, "offset", "has_more"}`。没有匹配时返回空列表，不是错误 |
| CAT-05 | `gateway_describe_tool`，参数 `{name: string}`，只读。结果文本与 `Structured` 都是该工具按 TL-02 输出的完整对象（含 TL-04 追加的可用服务列表）。工具不存在或对该客户端不可见 → `isError`，文本 `unknown tool <name>; use gateway_search_tools`（与真正不存在时相同，不泄露其他客户端的工具） |
| CAT-06 | `gateway_call_tool`，参数 `{name: string, arguments?: object}`，**非只读**（`readOnlyHint: false`）。行为与用 `name` 和 `arguments` 调用 `tools/call` 完全相同（TL-06 ~ TL-12）；工具不存在或不可见 → `isError`，文本同 CAT-05；`name` 是目录工具 → `isError`。不嵌套产生新的鉴权、限流或计费 |
| CAT-07 | 无论哪种模式，`tools/call` 都接受对该客户端可见的真实工具名（隐藏不等于禁用）；也接受目录工具名，但只在目录模式下：直接模式下调用目录工具 → `-32602 unknown tool` |
| CAT-08 | 目录工具的参数不合法（类型错、`limit` 越界等）→ `isError`，文本说明哪个参数不合法。日志只记目录工具名和目标工具名，不记 `query` 与 `arguments` |
| CAT-09 | DEPLOYMENT.md 的 MCP 一节说明 `tool_mode` 三种取值、`auto_threshold` 与 `pinned_tools` 的用法、目录模式下 AI 的三步调用过程，以及上下文开销的对比 |

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
| CLI-08 | `gateway tools`：`tools/list` 中含 `gateway_search_tools` 时，改为调用它（`query` 为空，`limit` 100，按 `has_more` 翻页）列出全部真实工具，输出格式同 CLI-06；`--json` 输出 `Structured` 中合并后的 `tools` 列表 |
| CLI-09 | `gateway keys import --pool P --file F [--format jsonl\|lines] [--field api_key] [--where k=v]... [--secret-prefix S] [--name-prefix N] [--weight W] [--max-in-flight M] [--dry-run]`：连接信息来自 `UAIG_ADMIN_URL`、`UAIG_ADMIN_TOKEN`（同 HR-02）。`jsonl`（缺省）每行一个 JSON 对象，取 `--field` 字段（缺省 `api_key`，必须是非空字符串），`--where` 要求顶层字段的字符串形式等于给定值（可重复，全部满足才导入）；`lines` 每行一个密钥，忽略空行与 `#` 开头的行。密钥前加 `--secret-prefix`（例如 `bearer:`）。名称为 `--name-prefix`（缺省为池名）+ `-` + 三位序号（按导入顺序）。文件上限 16 MiB。逐个 `POST /admin/api/keys`：201 计为导入，409 计为重复，其他错误计为失败并在 stderr 写行号与 HTTP 状态（不写错误体以外的内容）。输出 `imported N, duplicate D, skipped S (filtered or invalid), failed F`；有失败时退出码 1。`--dry-run` 只解析并计数，不调用管理 API。**任何输出都不包含密钥或文件中的其他字段值**。 |
| CLI-11 | `keys import` 的元数据参数（T62）：`--expires-field F`（jsonl）读取过期时间，接受 RFC 3339、Unix 秒（≥ 10^11 视为毫秒）、`YYYY-MM-DD HH:MM:SS`、`YYYY-MM-DDTHH:MM:SS`、`YYYY-MM-DD HH:MM`、`YYYY-MM-DD`（当天 0 点）；字段缺失、空串或 null 表示不过期；无法解析时跳过该行（原因 `bad <字段>`）。不带时区的时间按 `--tz`（`±HH:MM`、`UTC`、IANA 名称；缺省本机时区）解释。已过期的行默认跳过并计入 `expired`（原因 `expired`），`--include-expired` 照样导入。`--label L` 给全部 Key 打标签，`--label-field F` 取行内字段（二者互斥，≤ 64 字节）。`--update`：遇到 409 时用响应中的 `key_id` 以 `PATCH` 更新过期时间（文件中没有则清除）与标签，计入 `updated`；需要至少一个元数据参数。输出在 CLI-09 的计数之后追加 `, updated U, expired E`。参数组合非法时退出码 2 |
| CLI-10 | `gateway admin-mcp`：stdio MCP 服务端，桥接到 `<UAIG_ADMIN_URL>/admin/mcp`，Token 取 `UAIG_ADMIN_TOKEN`；行为同 CLI-04。 |

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

## 6. 管理面 MCP（任务卡 T58）

管理网关本身的 MCP 端点 `POST /admin/mcp`，只监听在管理面（回环地址），需要管理员 Token。只暴露不产生、不接收凭据的操作：新建客户端、轮换 Token、添加 Key 不提供工具（分别用 Web UI 与 `gateway keys import`，后者在本机读取账号文件，密钥不经过 AI）。

| 编号 | 规则 |
|:--|:--|
| AM-01 | 传输与方法同 SRV-M02 ~ SRV-M07（版本协商、`MCP-Protocol-Version` 校验、单条 JSON-RPC、不使用 SSE、`Origin` 校验）；`serverInfo.name` 为 `universal-ai-gateway-admin`；`instructions` 说明这些工具管理网关本身、写操作立即生效、新建客户端与添加 Key 请用 Web UI 与 `gateway keys import`。 |
| AM-02 | 工具（名称 → 管理 API）：`gateway_admin_status` → `GET health`；`gateway_admin_list_clients` → `GET clients`；`gateway_admin_list_keys` → `GET keys`；`gateway_admin_usage {client?, from?, to?}` → `GET usage`；`gateway_admin_config` → `GET config`；`gateway_admin_tools` → `GET tools`；`gateway_admin_reload_records` → `GET reload`；`gateway_admin_update_client {id, name?, quota_unlimited?, quota_nano_usd?, rpm_limit?, allowed_models?, disabled?}` → `PATCH clients/{id}`；`gateway_admin_set_key_state {id, state: ready\|disabled}` → `PATCH keys/{id}`；`gateway_admin_reload {dry_run?}` → `POST reload`。前 7 个 `readOnlyHint: true`，后 3 个 `false`（`set_key_state` 与 `update_client` 另有 `destructiveHint: true`）。 |
| AM-03 | 不提供新建客户端、轮换 Token、添加 Key 的工具（§2.2）。 |
| AM-04 | 调用：参数按工具的 Schema 校验类型（不合法 → `isError`）；构造进程内请求，复制外层的 `Authorization`、`RemoteAddr`、`Host`，交给管理面处理器，响应写入上限 1 MiB 的缓冲区。2xx → 文本为响应体（JSON），对象同时放进 `structuredContent`；非 2xx → `isError`，文本 `HTTP <状态>: <响应体>`。重载工具收到 202 时原样返回（进程内重启已开始，用 `gateway_admin_reload_records` 查看结果）。 |
| AM-05 | 每次 `tools/call` 记 Info 日志 `admin mcp call`：工具名、结果状态码；不记参数与响应。 |
