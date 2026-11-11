# T56 ~ T58 AI 原生的接入与管理（M3）

- **类型**：规格 + 实现
- **前置**：T54、T55（`DONE`）。不改内核（`internal/core`）与契约（`internal/protocol`），不改分层规则，不加依赖。
- **授权**：2026-11-11 用户要求完成四项：修复孤立 Key 的显示、接入 API 的 skill 与密钥导入命令、Web UI 补齐 T54 / T55、管理面 MCP。设计按下文的保守方案执行，规则写入 spec/09、spec/14。

| 卡 | 内容 |
|:--|:--|
| T56 | `gateway keys import`（密钥只在本机进程之间传递，不经过 AI）与接入 API 的 skill（`.claude/skills/add-api/`） |
| T57 | Web UI：修复孤立 Key 显示为「可用」；新增「配置」页（重载预览与应用、重载记录、服务与工具列表）；管理 API `GET /admin/api/tools` |
| T58 | 管理面 MCP：`POST /admin/mcp` 与 `gateway admin-mcp`（stdio 桥接） |

## 1. 背景

使用服务已经是 AI 原生的（模型后端、`/mcp`、接口声明、工具目录、命令行）。管理网关还不是：

1. 加 Key 要在管理 API 的请求体里传明文 `secret`。让 AI 帮忙加 Key，所有 Key 都会进入对话记录。
2. 管理操作没有工具描述，AI 只能自己拼 curl。
3. Web UI 停在 T54 之前：孤立 Key 显示成绿色的「可用」，没有重载入口，看不到工具。

## 2. 关键设计

### 2.1 密钥不经过 AI（T56）

`gateway keys import` 由网关命令行进程在本机读取账号文件，逐个调用管理 API `POST /admin/api/keys`（本机回环地址）。AI 只负责运行命令，看到的输出只有计数和行号，**永远没有 Key 本身**。不新增管理 API：写库、加密、去重（指纹冲突 409）、调度器刷新全部沿用 ADM-13、ADM-14。

Key 的名称不取自文件（账号文件里常有邮箱等个人信息），由 `--name-prefix` 加三位序号生成。

### 2.2 管理面 MCP 不经手凭据（T58）

管理面 MCP 只暴露**不产生、不接收凭据**的操作：

| 暴露 | 不暴露（原因） |
|:--|:--|
| 状态、客户端列表、Key 列表（只有 `secret_hint`）、用量、配置、工具、重载记录；修改客户端的额度 / 速率 / 允许的模型 / 禁用；启用或禁用 Key；重载配置（含 dry-run） | 新建客户端、轮换 Token（会把新 Token 交给 AI）；添加 Key（会把密钥交给 AI）：分别用 Web UI 与 `gateway keys import` |

实现上，每个工具都转换成一个**进程内的管理 API 请求**，带上外层请求的 `Authorization`、来源地址与 Host，交给完整的管理面处理器。鉴权、IP 锁定、参数校验、错误格式与直接调用管理 API 完全相同，不另写一套逻辑。

分层：`internal/admin` 与 `internal/mcp` 同在第 4 层、不能互相导入（分层规则不改）。管理面 MCP 在 `cmd/gateway` 中实现，用 `mcp` 包导出的 JSON-RPC 编解码；`admin.Deps.MCP` 把它挂在 `/admin/mcp`，因此先经过管理面的鉴权（管理员 Token、Host 检查、IP 锁定、1 MiB 请求体上限）。

### 2.3 Web UI（T57）

- 孤立 Key：状态显示「孤立」（灰色），原因「池已从配置中删除」，不显示启用 / 禁用按钮；概览计入「已停用」类。
- 「配置」页：重载预览（dry-run）与应用、进程内重启时等待完成、最近 20 次重载记录；各服务及其工具（含内置工具与接口声明）、MCP 工具列表模式。
- 新的管理 API `GET /admin/api/tools`：注册表中的全部工具（随热重载更新），供页面与管理面 MCP 使用。

## 3. 规则

### 3.1 命令行（spec/14 §3）

| 编号 | 规则 |
|:--|:--|
| CLI-09 | `gateway keys import --pool P --file F [--format jsonl\|lines] [--field api_key] [--where k=v]... [--secret-prefix S] [--name-prefix N] [--weight W] [--max-in-flight M] [--dry-run]`：连接信息来自 `UAIG_ADMIN_URL`、`UAIG_ADMIN_TOKEN`（同 HR-02）。`jsonl`（缺省）每行一个 JSON 对象，取 `--field` 字段（缺省 `api_key`，必须是非空字符串），`--where` 要求顶层字段的字符串形式等于给定值（可重复，全部满足才导入）；`lines` 每行一个密钥，忽略空行与 `#` 开头的行。密钥前加 `--secret-prefix`（例如 `bearer:`）。名称为 `--name-prefix`（缺省为池名）+ `-` + 三位序号（按导入顺序）。文件上限 16 MiB。逐个 `POST /admin/api/keys`：201 计为导入，409 计为重复，其他错误计为失败并在 stderr 写行号与 HTTP 状态（不写错误体以外的内容）。输出 `imported N, duplicate D, skipped S (filtered or invalid), failed F`；有失败时退出码 1。`--dry-run` 只解析并计数，不调用管理 API。**任何输出都不包含密钥或文件中的其他字段值**。 |
| CLI-10 | `gateway admin-mcp`：stdio MCP 服务端，桥接到 `<UAIG_ADMIN_URL>/admin/mcp`，Token 取 `UAIG_ADMIN_TOKEN`；行为同 CLI-04。 |

### 3.2 管理 API（spec/09 §4）

| 编号 | 规则 |
|:--|:--|
| ADM-20 | `GET /admin/api/tools`：`{"tool_mode": mcp.tool_mode, "tools": [{"name", "title"?, "summary", "service"?, "protocol", "read_only"}]}`，按名称升序；`summary` 为描述首行；`service` 为工具固定的服务名（空表示由参数选择）。来源是当前一代的注册表，热重载后立即更新。 |
| ADM-21 | `POST /admin/mcp`：管理面 MCP（spec/14 §6），经过管理面的全部中间件。`GET` / `DELETE` → 405。 |

### 3.3 管理面 MCP（spec/14 新增 §6）

| 编号 | 规则 |
|:--|:--|
| AM-01 | 传输与方法同 SRV-M02 ~ SRV-M07（版本协商、`MCP-Protocol-Version` 校验、单条 JSON-RPC、不使用 SSE、`Origin` 校验）；`serverInfo.name` 为 `universal-ai-gateway-admin`；`instructions` 说明这些工具管理网关本身、写操作立即生效、新建客户端与添加 Key 请用 Web UI 与 `gateway keys import`。 |
| AM-02 | 工具（名称 → 管理 API）：`gateway_admin_status` → `GET health`；`gateway_admin_list_clients` → `GET clients`；`gateway_admin_list_keys` → `GET keys`；`gateway_admin_usage {client?, from?, to?}` → `GET usage`；`gateway_admin_config` → `GET config`；`gateway_admin_tools` → `GET tools`；`gateway_admin_reload_records` → `GET reload`；`gateway_admin_update_client {id, name?, quota_unlimited?, quota_nano_usd?, rpm_limit?, allowed_models?, disabled?}` → `PATCH clients/{id}`；`gateway_admin_set_key_state {id, state: ready\|disabled}` → `PATCH keys/{id}`；`gateway_admin_reload {dry_run?}` → `POST reload`。前 7 个 `readOnlyHint: true`，后 3 个 `false`（`set_key_state` 与 `update_client` 另有 `destructiveHint: true`）。 |
| AM-03 | 不提供新建客户端、轮换 Token、添加 Key 的工具（§2.2）。 |
| AM-04 | 调用：参数按工具的 Schema 校验类型（不合法 → `isError`）；构造进程内请求，复制外层的 `Authorization`、`RemoteAddr`、`Host`，交给管理面处理器，响应写入上限 1 MiB 的缓冲区。2xx → 文本为响应体（JSON），对象同时放进 `structuredContent`；非 2xx → `isError`，文本 `HTTP <状态>: <响应体>`。重载工具收到 202 时原样返回（进程内重启已开始，用 `gateway_admin_reload_records` 查看结果）。 |
| AM-05 | 每次 `tools/call` 记 Info 日志 `admin mcp call`：工具名、结果状态码；不记参数与响应。 |

### 3.4 Web UI（spec/09 §6）

| 编号 | 规则 |
|:--|:--|
| UI-30 | Key 的 `state` 为 `orphaned` 时显示「孤立」（灰色），原因「池已从配置中删除」，不提供启用 / 禁用按钮；概览把它计入已停用。 |
| UI-31 | 「配置」页：按钮「预览重载」（`dry_run: true`）与「应用重载」（二次确认）；显示模式、增删改的池与服务、变化的设置、需要重启的字段、警告；收到 202 时显示「正在重启」，每秒查询 `GET reload` 直到该记录不再是 `restarting`（最多 2 分钟；重启期间的请求失败视为尚未就绪）；错误按 UI-16 显示。下方是最近 20 次重载记录。 |
| UI-32 | 「配置」页的服务与工具：按 `GET config` 列出池与服务，按 `GET tools` 列出工具（名称、所属服务、只读与否、说明首行）与当前的 `tool_mode`。全部用 `textContent` 写入（UI-12）。 |

## 4. 测试要求

1. **keys import**：jsonl 与 lines 两种格式；`--where` 过滤；字段缺失、非字符串、坏 JSON 行计为 skipped 并报告行号；`--secret-prefix`；重复导入计为 duplicate；`--dry-run` 不发请求；**stdout 与 stderr 中不出现任何密钥和文件中的其他值**（邮箱、密码）；缺少 `UAIG_ADMIN_TOKEN` 退出码 2；文件超过上限报错。
2. **管理面 MCP**：`initialize`、`tools/list`（10 个工具、只读标记）；每个工具到达正确的管理 API（端到端）；参数类型错误 → `isError`；管理 API 4xx → `isError` 且含错误体；非管理员 Token → 403；不存在新建客户端 / 添加 Key 的工具；日志中没有参数；`gateway admin-mcp` 桥接可用。
3. **tools API**：列出内置工具与接口声明；热重载新增接口后立即出现。
4. **Web UI**：`orphaned` 的显示与按钮（`ui_test.go` 中对静态文件的检查）；新页面不使用 `innerHTML`。
5. `go run ./tools/check` 全绿。

## 5. 验收标准

- [x] §3 写入 spec；spec/11 中 T56 ~ T58 为 `DONE`。
- [x] §4 测试全部存在并通过；`go run ./tools/check` 全绿。Web UI 只做了静态检查（UI-12 规则、文案中英文对齐、JS 语法），没有在浏览器中实际操作。
- [x] 实测（2026-11-11，12/12 通过）：从真实账号文件导入 94 把 Bivia Key，再次导入全部计为重复；输出与网关日志中没有文件里的任何值（逐一比对 399 个敏感值）；用导入的 Key 调用成功；管理面 MCP 查询状态与 Key 列表（无密钥）、停用再启用一把 Key、dry-run 重载、查询用量。
- [x] DEPLOYMENT.md 写明 `gateway keys import`（§9.2）、管理面 MCP（§6.2）与「配置」页（§8.1）；skill（`.claude/skills/add-api/SKILL.md`）写明工作流与安全底线。
