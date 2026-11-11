# T54 rest 接口声明与 MCP 工具目录（M3）

- **类型**：规格 + 实现
- **前置**：T51（`DONE`）。不改内核（`internal/core`）与契约（`internal/protocol`），不加依赖。
- **需要用户审批**：~~是~~ **已批准**（2026-11-11）：7 个待定项全部采用建议方案 **a**（见 §3.6「决议」列）。阶段 A 按 §3 把规格写入 spec/03、spec/08 §8.3.5、spec/14 §2.4 ~ §3，提交信息不写「待审批」；之后直接进入阶段 B。
- **必读**：
  - [`spec/14_ai_access.md`](../../spec/14_ai_access.md) §1（TS-01 ~ TS-06，`ToolSource` 契约）、§2（SRV-M01 ~ SRV-M08、TL-01 ~ TL-11）、§3（CLI-01 ~ CLI-07）、§5
  - [`spec/08_adapters.md`](../../spec/08_adapters.md) §8.3（RS-01 ~ RS-31）
  - [`spec/03_config.md`](../../spec/03_config.md) `models[]`、`mcp` 与配置校验（CFG-xx）
  - `internal/protocol/tool.go`、`internal/protocol/registry.go`（`indexTools`，REG-05）
  - `internal/mcp/`：`server.go`（`Deps`、`dispatch`）、`invoke.go`（`listTools`、`callTool`、`fillPath`）
  - `internal/adapter/rest/`、`internal/adapter/tavily/ingress.go`（`ToolSource` 范例）
  - `cmd/gateway/adapters.go`（`newRegistry`）、`cmd/gateway/serve.go`（`mcp.New`）、`cmd/gateway/client_cmds.go`、`internal/config/`

## 1. 背景与目标

T51 之后，任何 REST API 改配置就能接入网关，但 AI 助手用不上它：MCP 工具只来自适配器代码里写死的 `Tools()`（Tavily、MinerU 等），`rest` 一个工具也不提供。

如果简单地把每个接口都变成一个 MCP 工具，又会出现第二个问题：客户端一连上就调用 `tools/list`，把每个工具的描述和参数 Schema 全部放进 AI 的上下文（一个工具大约 150–300 token）。接口越接越多，常驻上下文跟着线性增长，AI 选错工具的概率也会上升。

本卡解决这两个问题：

1. **接口声明**：管理员在配置里给 rest 服务声明接口（名字、说明、方法、路径、参数 Schema），它们成为注册表里的工具，调用时照常鉴权、计费、选 Key、重试。
2. **工具目录**：MCP 服务端可以不直接列出全部工具，而是只给 AI **3 个固定的目录工具**：搜索、查看说明、调用。AI 先搜索、再看说明、最后调用，常驻上下文的大小与接口数量无关。目录在 MCP 层实现，与协议无关，Tavily、MinerU 等内置工具也一并纳入。

```yaml
mcp:
  tool_mode: auto            # auto | direct | catalog
  auto_threshold: 8          # auto：客户端可见的工具不超过 8 个时直接列出，否则切换为目录
  pinned_tools: [tavily_search]   # 目录模式下仍然直接列出的常用工具

pools:
  - name: bivia-pool
    protocol: rest
    base_url: "https://api.articul8.ai/bivia"

models:
  - name: bivia
    pool: bivia-pool
    price: { requests: "0.001" }
    reserve: { requests: 1 }
    tools:
      - name: bivia_boolean
        description: "对一段情境回答是或否的问题，返回答案、概率和是否弃答。适合审批、合规检查一类二元判断。"
        method: POST
        path: /boolean
        idempotent: true          # 无副作用的 POST：走 /rest-idem/，失败可换 Key 重试
        read_only: true
        input_schema:
          type: object
          properties:
            state:    { type: string, description: "需要判断的情境或事实" }
            question: { type: string, description: "要回答的是非问题" }
            mode:     { type: string, enum: [fast, accurate], description: "fast 更快，accurate 更准" }
          required: [state, question]
          additionalProperties: false

  # 另一个服务（池与价格从略）：GET 接口，{x} 从参数中取值，剩余参数进查询串
  - name: github-ro
    pool: github-ro-pool
    tools:
      - name: github_issue_get
        description: "读取一个 GitHub issue 的标题、正文、状态和标签。"
        method: GET
        path: /repos/{owner}/{repo}/issues/{number}
        input_schema:
          type: object
          properties:
            owner:  { type: string, description: "仓库所有者" }
            repo:   { type: string, description: "仓库名" }
            number: { type: integer, description: "issue 编号" }
          required: [owner, repo, number]
          additionalProperties: false
```

目录模式下 AI 的使用过程：

```
gateway_search_tools {"query": "判断"}
  → bivia_boolean — 对一段情境回答是或否的问题……（每个工具一行）
gateway_describe_tool {"name": "bivia_boolean"}
  → 完整的参数 Schema
gateway_call_tool {"name": "bivia_boolean", "arguments": {"state": "...", "question": "..."}}
  → 调用结果
```

## 2. 关键设计

### 2.1 接口声明来自配置，在装配时注入

`ToolSource` 是入站适配器的可选接口，注册表在 `Seal` 时调用 `Tools()` 建立工具索引（REG-05）。工具表必须在注册之前就确定，因此：

- `cmd/gateway/adapters.go` 的 `newRegistry` 已经拿到 `cfg`。它从配置收集全部接口声明，改为调用 `rest.RegisterWithOptions(reg, rest.Options{Logger, Tools})` 注册 rest，不再走 `adapterRegistrars` 列表里的 `rest.Register`。
- `rest.Ingress` 增加一个不导出的字段，指向**构造后不再修改**的工具表，因此仍是无状态、并发安全的。零值 `Ingress{}` 没有工具，行为与 T51 完全相同；一致性套件和现有测试不受影响。
- 配置包只做与协议无关的校验（名称、方法、路径格式、Schema 的形状）。「接口只能声明在 rest 池的服务上」由装配代码检查，配置包不出现协议名。

配置变化需要重启才生效，和 `models` 一样。

### 2.2 参数怎么变成请求

不引入 JSON Schema 校验库（禁止新增依赖），映射规则固定且简单：

1. 参数必须是 JSON 对象（缺省或 `null` 视为 `{}`）。
2. `path` 中的 `{name}` 从同名参数取值，并从参数表中移出：值必须是字符串或数字，按单个路径段转义（`url.PathEscape`）；空串、`.`、`..` 拒绝。
3. `GET` / `DELETE`：其余参数放进查询串。标量各取一个值，标量数组重复同名参数，`null` 省略；对象或嵌套数组拒绝。
4. `POST` / `PUT` / `PATCH`：其余参数原样组成 JSON 对象作为请求体（数字保持原文，不经 float64 转换）。

`input_schema` 原样给 AI 看，用来引导它填参数；网关不按它做完整校验，参数是否合法由上游判断，上游的错误消息会作为工具错误返回给 AI。以后需要「参数放在请求头里」之类的显式位置时，可以兼容地增加 `in` 字段（见待定 2）。

### 2.3 有副作用的接口

`method: POST` / `PATCH` 时，`idempotent: true` 表示走 `/rest-idem/` 的 Op（可换 Key 重试，RS-19），否则走 `/rest/`（只尝试一次）。规则与客户端直接调用时一样：**是否可以安全重试，只有配置者知道**。

`read_only` 映射到 MCP 的 `readOnlyHint`。缺省值是：`GET` 为 true，其他方法为 false。直接模式下，大多数 MCP 客户端会对非只读工具先征求用户确认。目录模式下 AI 统一通过 `gateway_call_tool` 调用，客户端看不到被调用接口的 `readOnlyHint`，所以 `gateway_call_tool` 本身标记为非只读，`gateway_describe_tool` 的结果里写明被调用接口是否只读（CAT-05、CAT-06）。

### 2.4 工具目录：常驻上下文与接口数量无关

目录完全在 `internal/mcp` 实现，只用到注册表的 `Tools()` 和现有的 `callTool`，不涉及协议知识：

- **可见性不变**：搜索、查看、调用都只针对该客户端按 TL-03 / TL-04 可见的工具；调用时内核照常检查白名单、计费、选 Key。目录工具本身不访问上游、不计费。
- **模式按客户端计算**：`auto` 模式按**该客户端可见的工具数**判断，因为不同客户端的白名单不同，看到的工具数也不同。
- **隐藏不等于禁用**：目录模式下，真实工具不出现在 `tools/list` 里，但仍然可以用原名通过 `tools/call` 直接调用（待定 7）。这样 `gateway call <工具名>` 命令行和老配置继续可用。
- **常用工具置顶**：`mcp.pinned_tools` 中的工具在目录模式下仍然直接列出，省去一两步查找。
- **代价**：AI 调用一个不常用的接口要多走一两步（搜索、查看），换来上下文可控。

常驻上下文估算：目录模式下 `tools/list` 只有 3 个目录工具加上置顶工具，大约 600 token；一次调用额外加载一次搜索结果（每个工具一行）和一个工具的 Schema。

### 2.5 接口不是新的权限边界

声明接口让 AI **方便**地调用它，但不**限制**它：只要客户端的 `allowed_models` 允许 `bivia`，它照样可以直接请求 `/rest/bivia/` 下的任意路径（T51 §2.5、RS-30）。权限仍然靠最小权限的 Key 和服务名拆分来控制。部署文档必须写明这一点，不能让人误以为「只声明了只读接口就安全」。

### 2.6 顺带修复：MCP 内部请求把 `{path...}` 里的斜杠转义了

`internal/mcp/invoke.go` 的 `fillPath` 对每个通配段整体执行 `url.PathEscape`。`{path...}` 的值 `repos/a/b` 会变成 `repos%2Fa%2Fb`，rest 适配器取 `EscapedPath` 后原样转发，上游收到的就是错误路径；空的 `{path...}`（访问上游根路径）还会报 `missing path parameter`。现有适配器的工具都没有用到 `...` 通配段，所以这个问题一直没暴露。本卡修复：`{name...}` 的值约定为**已转义的路径**，校验后原样写入，允许为空（调用方负责转义每个段，这样段内的 `/` 可以表示为 `%2F`）；`{name}` 的行为不变（现有测试 `fillPath("/v1/responses/{id}/cancel", {"id": "r/1"})` 继续期望 `r%2F1`）。

## 3. 规格草案（阶段 A：写入 spec，提交审批）

### 3.1 配置（spec/03，新增 CFG-17 ~ CFG-19）

| 编号 | 规则 |
|:--|:--|
| CFG-17 | `models[].tools` 可选，是接口声明的列表。每项字段：`name`（匹配 `^[A-Za-z0-9_.-]{1,64}$`，在整份配置内唯一）、`title`（可选）、`description`（非空）、`method`（`GET`、`DELETE`、`POST`、`PUT`、`PATCH` 之一，不区分大小写，规范化为大写）、`path`（以 `/` 开头；不含 `?`、`#`、`\`、NUL；按 `/` 拆段后没有 `.` 或 `..` 段；`{name}` 占位符的名字匹配 `^[A-Za-z_][A-Za-z0-9_]*$`，同一名字只出现一次，不允许 `{name...}`）、`input_schema`（可选；缺省为 `{"type":"object","properties":{},"additionalProperties":false}`；给出时必须是映射，`type` 为 `object`，键名都是字符串；每个占位符必须出现在 `properties` 与 `required` 中）、`idempotent`（可选，布尔，只允许用于 `POST` / `PATCH`）、`read_only`（可选，布尔；缺省 `GET` 为 true，其他为 false）。任一项不满足 → 拒绝启动，错误消息含 `models[i].tools[j]` 位置 |
| CFG-18 | `models[].tools` 非空时，该服务所在池的协议必须支持接口声明（目前只有 `rest`），否则拒绝启动。由装配代码（`cmd/gateway`）检查，配置包不判断协议名 |
| CFG-19 | `mcp.tool_mode` 取 `auto`（缺省）、`direct`、`catalog` 之一；`mcp.auto_threshold` 缺省 8，必须 `>= 1`；`mcp.pinned_tools` 可选，每一项必须是注册表中存在的工具名，且不能是目录工具名（CAT-01），否则拒绝启动（工具名的存在性在注册表封存后由装配代码检查） |

### 3.2 rest 适配器（spec/08 §8.3，新增 §8.3.5）

| 编号 | 规则 |
|:--|:--|
| RS-40 | `rest.Options{Logger *slog.Logger; Tools []ToolDef}`；`RegisterWithOptions(r, opts)`。`ToolDef` 是配置中接口声明的适配器侧表示，另含 `Service`（所属 `models[].name`）。`Register`、`RegisterWithLogger` 保留，等价于没有工具。`Ingress` 实现 `protocol.ToolSource`；零值 `Ingress{}` 的 `Tools()` 返回空 |
| RS-41 | `Tools()`：每个声明生成一个 `protocol.Tool`，`Model` = `Service`，`ReadOnly` 按 CFG-17 的缺省规则，`InputSchema` 为声明的 Schema 编码成的 JSON；按名称升序。工具名与其他适配器冲突时由 `Registry.Seal` 报错，网关拒绝启动（REG-05） |
| RS-42 | `ToolRequest(name, args)` 按 §2.2 映射。Op：`GET` → `rest.get`，`DELETE` → `rest.delete`，`PUT` → `rest.put`，`POST` → `idempotent` ? `rest.post_idem` : `rest.post`，`PATCH` 同理。`PathValues` = `{"service": Service, "path": 去掉开头 / 并填好占位符（已转义）的路径}`。参数不合法 → `CodeInvalidRequest`，消息指明参数名，如 `argument "owner" must be a string or number` |
| RS-43 | `ToolResult`：2xx 时，响应体为空 → 文本 `empty response (HTTP <status>)`；`Content-Type` 为 `application/json` 或 `*+json` 且是合法 JSON → 文本为响应体原文，顶层是对象时同时放进 `Structured`；`text/*` → 文本为响应体（必须是合法 UTF-8，否则同下）；没有 `Content-Type` → 按文本处理（不少 API 不设置它），是合法 JSON 对象时同样放进 `Structured`；其他类型 → `IsError`，文本 `binary response (<Content-Type>, <N> bytes) is not supported by tools`，不包含响应内容。非 2xx → `IsError`，文本 `HTTP <status>: ` + 响应体前 2000 字节（在 UTF-8 字符边界截断）。`ToolResult` 不返回 error |
| RS-44 | 工具调用与直接调用同一路由，鉴权、白名单（TL-03 / TL-06）、计费、选 Key、重试、403 告警全部沿用 T51 规则，本卡不增加特殊路径 |

### 3.3 MCP 内部请求（spec/14 §2.4，新增 TL-12）

| 编号 | 规则 |
|:--|:--|
| TL-12 | `fillPath`：`{name...}` 通配段的值是**已转义的路径**（与 `URL.EscapedPath()` 同一形式）：必须能被 `url.PathUnescape` 解码，且不含 `?`、`#`、空白与控制字符，否则 `-32602`；合法时原样写入，允许为空；内部请求中该名字的 `PathValue` 为解码后的值。`{name}` 的值仍整体 `url.PathEscape` 且不允许为空 |

### 3.4 工具目录（spec/14，新增 §2.5）

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
| CLI-08 | `gateway tools`：`tools/list` 中含 `gateway_search_tools` 时，改为调用它（`query` 为空，`limit` 100，按 `has_more` 翻页）列出全部真实工具，输出格式同 CLI-06；`--json` 输出 `Structured` 中合并后的 `tools` 列表 |

### 3.5 部署文档

| 编号 | 规则 |
|:--|:--|
| RS-45 | DEPLOYMENT.md §9 新增「把接口暴露给 AI」：配置示例（Bivia 的 POST 接口、带占位符的 GET 接口）、参数映射规则、`idempotent` 与 `read_only` 的含义、二进制响应不支持、修改后需重启；并醒目写明 §2.5：接口声明不是权限边界 |
| CAT-09 | DEPLOYMENT.md 的 MCP 一节说明 `tool_mode` 三种取值、`auto_threshold` 与 `pinned_tools` 的用法、目录模式下 AI 的三步调用过程，以及上下文开销的对比 |

### 3.6 待定项汇总

| # | 问题 | a（建议） | b | 决议 |
|:--|:--|:--|:--|:--|
| 1 | 接口声明放在哪里 | **a**：`models[].tools`，跟着服务走，`Model` 自然确定 | 顶层 `tools:`，每项带 `service` 字段 | **a** |
| 2 | 参数映射方式 | **a**：按 §2.2 自动映射（占位符、查询串、JSON 体）；以后需要时兼容地加 `in` 字段 | 每个参数显式声明 `in: path / query / body / header`（类似 OpenAPI），配置更啰嗦 | **a** |
| 3 | 写操作接口 | **a**：允许；`read_only` 缺省为 false | 只允许 `GET` 和 `idempotent: true` 的 `POST` / `PATCH` | **a** |
| 4 | 二进制响应 | **a**：拒绝并返回工具错误 | `image/*` 转成 MCP 图像内容（base64），其他类型仍拒绝 | **a** |
| 5 | `tool_mode` 缺省值 | **a**：`auto`，阈值 8：工具少时最顺手，多了自动变省 | `catalog`：始终省上下文 / `direct`：与现状完全一致 | **a** |
| 6 | 搜索的匹配方式 | **a**：按词的子串匹配（全部词都出现），按名称排序：简单、结果确定、易测试 | 带打分的模糊匹配（名称命中优先），实现与测试更复杂 | **a** |
| 7 | 目录模式下能否用原名直接调用隐藏的工具 | **a**：能（CAT-07）：命令行和老配置继续可用，可见性与白名单不变 | 不能：只能通过 `gateway_call_tool` 调用 | **a** |

> 2026-11-11 用户批准：全部采用 a。

## 4. 实现要求（阶段 B：用户批准后）

### 4.1 文件布局

```
internal/config/
  config.go         ModelConfig.Tools []ToolConfig（CFG-17）；MCPConfig 增加 ToolMode、AutoThreshold、PinnedTools（CFG-19）
  load.go           校验 CFG-17、CFG-19（工具名存在性除外）
internal/adapter/rest/
  register.go       Options、RegisterWithOptions（RS-40）
  tools.go          新增：ToolDef、Tools / ToolRequest / ToolResult（RS-41 ~ RS-43）
  ingress.go        Ingress 增加不导出的工具表字段
internal/mcp/
  server.go         Deps 增加 ToolMode、AutoThreshold、PinnedTools；dispatch 按 CAT-02、CAT-03、CAT-07 分派
  catalog.go        新增：三个目录工具（CAT-01、CAT-04 ~ CAT-06、CAT-08）
  invoke.go         fillPath（TL-12）；listTools 与可见性判断拆出可复用的函数供目录使用
cmd/gateway/
  adapters.go       收集接口声明、检查 CFG-18、改用 RegisterWithOptions
  serve.go          检查 pinned_tools 存在性与 CAT-01 冲突（CFG-19）；把 MCP 配置传给 mcp.New
  client_cmds.go    CLI-08
```

`mcp.Deps` 只**增加**字段，不改已有字段；零值（`ToolMode` 为空）等价于 `direct`，现有测试中构造的 `Deps` 行为不变。

### 4.2 约束

- 不改 `internal/core`、`internal/protocol`、`internal/archtest`、`internal/adapter/adaptertest`。
- JSON 请求体用 `json.RawMessage` 保留数字原文；参数解析使用 `json.Decoder.UseNumber` 或等价方式。
- 不在日志里记录工具参数、搜索词和响应内容（可能含用户数据）；需要排查时只记工具名、状态码和长度。
- 搜索在内存中遍历可见工具即可（工具数在数百以内）；不缓存跨客户端的结果。

## 5. 涉及文件

- `spec/03_config.md`、`spec/08_adapters.md`、`spec/14_ai_access.md`、`spec/11_tasks.md`（阶段 A）
- `internal/config/config.go`、`internal/config/load.go`、`internal/config/*_test.go`
- `internal/adapter/rest/register.go`、`ingress.go`、`tools.go`（新增）、`tools_test.go`（新增）
- `internal/mcp/server.go`、`invoke.go`、`catalog.go`（新增）、`catalog_test.go`（新增）、`mcp_test.go`
- `cmd/gateway/adapters.go`、`serve.go`、`client_cmds.go`、`client_cmds_test.go`、`e2e_rest_test.go`
- `docs/DEPLOYMENT.md`、`gateway.example.yaml`

## 6. 测试要求

1. **配置**：
   - CFG-17 每条约束一个反例：非法名、重名、非法方法、路径含 `..` / `?`、占位符缺 Schema 属性或不在 `required`、`idempotent` 用于 `GET`、Schema 的 `type` 不是 `object`、YAML 非字符串键；以及缺省 Schema 和 `read_only` 缺省值。
   - CFG-18：在 tavily 池的服务上声明接口会拒绝启动。
   - CFG-19：非法 `tool_mode`、`auto_threshold` 为 0、`pinned_tools` 含不存在的工具或目录工具名，都会拒绝启动。
2. **ToolRequest**：
   - 占位符取字符串和数字；
   - 拒绝空串、`.`、`..`、对象；
   - 斜杠被转义成一个段内的 `%2F`；
   - GET 的标量、数组、`null` 映射到查询串，对象被拒绝；
   - POST 剩余参数组成请求体，大整数 `12345678901234567890` 原文保留；
   - `idempotent` 选择正确的 Op；
   - 根路径 `/` 生成空的 `path`。
3. **ToolResult**：JSON 对象（含 `Structured`）、JSON 数组（无 `Structured`）、`text/plain`、空响应、二进制、非法 UTF-8、非 2xx 截断在字符边界。
4. **fillPath**：`{path...}` 多段保留斜杠、`%2F` 原样保留、允许为空；含 `?`、`#`、空格或非法百分号编码时报错；PathValue 为解码后的值；`{id}` 行为不变（现有用例保持通过）。
5. **目录**：
   - 三种模式下 `tools/list` 的内容；`auto` 恰好在阈值处和阈值加一处切换；两个白名单不同的客户端得到不同的模式；
   - 置顶工具只在对客户端可见时列出；`instructions` 只在目录模式下追加；
   - 搜索：多词匹配、大小写、空查询、分页与 `has_more`、描述首行截断、不返回其他客户端的工具和目录工具本身；
   - 查看与调用：不存在的工具和不可见的工具返回同一条消息；`gateway_call_tool` 的结果与直接 `tools/call` 逐字节相同；不能调用目录工具自身；
   - CAT-07：两种模式下按原名调用隐藏工具都可以；直接模式下调用目录工具得到 `-32602`；
   - 参数不合法返回 `isError`；日志中没有搜索词和参数。
6. **CLI-08**：目录模式下 `gateway tools` 翻页列出全部真实工具，`--json` 合并结果。
7. **端到端**（假上游）：
   - 直接模式下 `tools/list` 只对白名单内的客户端列出 rest 接口；
   - 通过 `tools/call` 与 `gateway_call_tool` 到达假上游的方法、路径、查询串、请求体都正确；
   - `idempotent` 接口遇 503 换 Key 重试后成功、只计费一次；
   - 非 idempotent 的 POST 接口遇 503 只尝试一次并返回 `isError`；
   - 上游 4xx 的错误体出现在工具结果中；
   - 目录工具的搜索与查看不产生计费事件；
   - `gateway call <接口名>` 在目录模式下仍然可用；
   - 日志中没有 Key 明文和工具参数。

## 7. 验收标准

- [x] 待定项已有用户决议（全部 a）。
- [x] 阶段 A：§3 写入 spec。
- [x] `go run ./tools/check` 全绿；§6 所列测试全部存在并通过。
- [x] 不改内核、契约、架构检查和一致性套件，不新增依赖。
- [x] 用真实 Bivia Key 实测（2026-11-11，12/12 通过）：目录模式下经「搜索 → 查看 → 调用」调用 `bivia_boolean` 得到结果；`gateway call bivia_score` 按原名直接调用可用；`gateway tools` 列出真实工具；缺参数时上游 422 作为工具错误返回；只有成功的 2 次调用计费。直接模式的工具列表由单元测试覆盖，未做真实 Key 实测。
- [x] DEPLOYMENT.md 按 RS-45、CAT-09 更新，含「接口声明不是权限边界」的提示。
- [x] spec/11 中 T54 状态改为 `DONE`。

## 8. 建议提交拆分

1. `docs(spec): T54 rest 接口声明与 MCP 工具目录规格`：阶段 A。
2. `fix(mcp): T54 通配路径段逐段转义`：TL-12 及测试。
3. `feat(rest): T54 配置驱动的接口声明`：config（CFG-17、CFG-18）、rest、装配及单元测试。
4. `feat(mcp): T54 工具目录模式`：CFG-19、CAT-01 ~ CAT-08、CLI-08 及测试。
5. `test(gateway): T54 端到端用例` 与 `docs: T54 部署文档`。
