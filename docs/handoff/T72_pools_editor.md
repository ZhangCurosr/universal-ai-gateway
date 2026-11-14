# T72 网页端管理池与服务（已批准，已完成）

- **类型**：规格 + 实现。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：T71（配置文件写回，`DONE`）、T55（配置重载，`DONE`）。
- **约束**：不改 `internal/core`、`internal/protocol` 的导出 API；不改分层规则；不加依赖。**配置文件仍然是唯一的事实来源**：网页上的修改写回 `gateway.yaml`，手工编辑与网页编辑互不冲突。

## 1. 要解决的问题

新建一个池（例如接入一家新的大模型或搜索 API）现在只能手工编辑 `gateway.yaml` 再到「配置」页重载。用户在网页上只能看到池与服务，不能新建、修改或删除。T71 已经能把单个值写回配置文件；本卡把它扩展到池（`pools[]`）与服务（`models[]`）的整项增删改。

## 2. 页面

「配置」页的「服务与工具」上方新增「池与服务」卡片（「上游 Key」页没有池时，空状态里也给一个「新建池」入口）：

1. **池列表**：名称、协议、上游地址、服务数、Key 数；每行「编辑」「删除」。按钮「新建池」。
2. **新建 / 编辑池**（对话框）：
   - 基本字段表单：名称（新建时填写，之后不能改）、协议（下拉，来自网关已注册的协议）、上游地址（选择协议后填入该协议的常用地址，可以改）、并发策略、最大尝试次数。
   - 「同时新建一个服务」（新建池时默认勾选）：服务名、上游模型名（可空）、**价格**——按所选协议的计量单位逐项列出输入框（例如 anthropic 的 4 种 token、tavily 的 `credits`、rest 的 `requests`），这样不会漏掉某个单位导致启动失败。
   - 「高级（YAML）」标签页：这一项的完整 YAML（超时、冷却、熔断、额度、告警、`api`、`tools`……都在这里改），表单与 YAML 两个标签页互相同步。
   - 按钮「检查」（只校验，不写文件）与「保存并生效」。
3. **服务列表与编辑**：同样的方式，表单字段为服务名、所属池、上游模型名、价格（按所属池的协议列出单位）、预留量；其余字段（`tools`、`api`）在 YAML 标签页。
4. **删除**：二次确认。池下还有服务时不能删（先删服务）；池里还有 Key 时说明「这些 Key 会成为孤立 Key，重新加回同名池即恢复」（沿用 T57 的孤立 Key 行为）。

## 3. 规则

| 编号 | 规则 |
|:--|:--|
| PE-01 | `GET /admin/api/config/entries`：`{"version": "<配置文件内容的 sha256>", "writable": bool, "pools": [{name, yaml, services, keys}], "models": [{name, pool, yaml}], "protocols": [{name, units: [...], default_base_url}]}`。`yaml` 是该项在配置文件中的原文（含项内注释）；`protocols` 来自注册表（单位即出站适配器的 `Units()`；rest 为 `requests` 加上 `api.endpoints[].usage` 中出现的单位）；常用地址由装配代码提供（anthropic → `https://api.anthropic.com`、openai_chat → `https://api.openai.com`、gemini → `https://generativelanguage.googleapis.com`、tavily → `https://api.tavily.com`、mineru → `https://mineru.net`，其余为空） |
| PE-02 | 写操作：`POST /admin/api/config/pools`、`PUT /admin/api/config/pools/{name}`、`DELETE /admin/api/config/pools/{name}`，服务同理（`/config/models`）。请求体 `{"version": "...", "yaml": "<该项的 YAML>", "check": bool}`（DELETE 只要 `version`）。`version` 与文件当前内容不一致 → 409（文件已被别人或手工改过，刷新后重试）。`check: true` 只校验并返回结果，不写文件 |
| PE-03 | 校验：`yaml` 必须是一个映射且只描述一项；名称与路径中的 `{name}` 一致（PUT 不能改名，改名等于删除再新建，由用户自己决定）；新建时名称不能已存在。把它放进完整配置后，先用 `config.Parse`（CFG 全部规则），再用与重载相同的构造检查（注册表、价目单位齐全 CORE-05、接口声明 CFG-18 等）。任何一步失败 → 400，消息指明原因，文件不变 |
| PE-04 | 写配置文件：用 `yaml.v3` 定位 `pools` / `models` 序列中的那一项，**只替换该项的文本行**（新建追加到序列末尾，删除去掉该项的行），文件其他部分（注释、空行、其他项）原样不变；`pools:` / `models:` 不存在时追加。备份 `<文件>.bak`、原子写入、重载失败恢复原文件，同 LAN-03 |
| PE-05 | 写入后按 HR 规则重载（池与服务的增删改是热重载，不重启）；响应 `{"version": "<新版本>", "reload": <ADM-18 的响应>}`。删除池时池下还有服务 → 400；删除服务时它是某个客户端 `allowed_models` 中的唯一一项 → 仍然允许，响应 `warnings` 中列出受影响的客户端 |
| PE-06 | 每次写入记 Info 日志 `config edited by admin`（`kind`、`name`、`op`、操作者），出现在重载记录中；不记 YAML 内容 |
| PE-07 | 配置文件不可写（`writable: false`）时页面只读，写操作 409。管理面 MCP **不**增加写配置的工具（只有网页与管理 API 能改配置；AI 改配置仍然走编辑文件 + `gateway reload`，可以审阅 diff） |

## 4. 改动文件

- `internal/config/edit.go`：新增按序列项增删改（`ReplaceListItem`、`AppendListItem`、`RemoveListItem`）与测试。
- `internal/admin`：新增 `config_edit.go`（PE-01 ~ PE-07）与测试；`Deps` 注入「检查」「写入并重载」「协议与单位」三个函数；UI：`pages/config.js`（池与服务卡片）、新增 `pages/config_editor.js`（对话框与表单 ↔ YAML 同步）、`pages/keys.js`（空状态入口）、`i18n.js`、`style.css`。
- `cmd/gateway`：`share.go` 的写回逻辑抽成通用的 `configEditor`（T71 与本卡共用）；新增 `checkConfigBytes`（解析 + 构造一代，不应用）；`serve.go` 注入。
- 规格：spec/09 新增 §9（PE-01 ~ PE-07）、UI-40；spec/03 CFG-24 扩展到池与服务；spec/11 T72；`docs/DEPLOYMENT.md` 加一段。

## 5. 测试

- 序列项编辑：替换中间一项、追加、删除第一 / 最后一项、项内与项外注释、`pools:` 不存在、流式写法（`pools: [...]`）拒绝。
- API：版本冲突 409、改名 400、重名 400、缺价目单位 400（文件不变）、`check` 不写文件、删除仍有服务的池 400、删除有 Key 的池（孤立 Key 警告）、不可写 409。
- 端到端：网页 API 新建 rest 池 + 服务 → 热重载 → 用客户端 Token 调用成功；编辑价格 → 计费按新价格；删除 → 请求 404 / unknown model。
- UI：表单 ↔ YAML 同步、按协议列出价格单位、中英文文案。

## 6. 实测（验收）

1. 在浏览器中新建 Tavily 池（协议下拉选 tavily，地址自动填入，价格只出现 `credits`），导入 Key 后搜索成功。
2. 在 YAML 标签页给池加上 `quota`，保存后「上游 Key」页显示额度列。
3. `gateway.yaml` 中只有新增 / 修改的那几行变化，注释保留，`.bak` 是修改前的内容。
4. 删除服务、删除池（Key 变为孤立，重新加回同名池后恢复）。

### 6.1 实测结果（2026-11-14，通过）

本机网关从带注释的配置文件启动（两个 rest 演示池），在内置浏览器中操作：

- [x] 「新建池」：名称 `tavily`，协议下拉选 `tavily` 后上游地址自动填入 `https://api.tavily.com`，「同时新建服务」只列出 `credits` 一个价格；YAML 标签页显示由表单生成的内容。保存后两次热重载（池、服务），表格立即更新。
- [x] `gateway.yaml` 只多了 6 行（池 3 行、服务 3 行），文件第一行的注释与其他内容不变；`.bak` 是最后一次写入前的内容。
- [x] 导入 86 把真实 Tavily Key 后 `POST /search` 成功（200）。
- [x] YAML 标签页给池加上带注释的 `quota`：文件原样写入（含项内注释），「上游 Key」页的池摘要显示「剩余 85,999 credits · 能撑到 2026/11/30 重置」。
- [x] 删除：池下还有服务时被拒绝；池上有 `quota` 时删除最后一个服务被拒绝（CFG-21：额度单位没有服务计价），提示清楚、文件不变；先在 YAML 中去掉 `quota`，再删服务、删池，配置文件与最初**完全相同**，86 把 Key 变为孤立（`orphaned`）。

## 7. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 表单覆盖哪些字段 | A. 表单只放常用字段，其余在同一对话框的「高级（YAML）」里改；B. 为所有字段做表单 | **A**：池与服务有几十个字段（超时、熔断、额度、告警、`api`、`tools`），全部做表单维护成本高、也难用；YAML 标签页保证什么都能改 |
| 2 | 改动怎么写回文件 | A. 只替换该项的文本行（PE-04），其他内容原样不变；B. 重新输出整个文件 | **A**：保留用户的注释与排版，`git diff` 只显示真正改动的几行 |
| 3 | 能否改名 | A. 不能（改名 = 删除 + 新建，Key 会变成孤立）；B. 能，自动迁移 Key | **A**：改名涉及 Key 的归属与客户端的 `allowed_models`，自动处理容易出错；不能改名最直观 |
| 4 | 并发修改 | A. 用文件内容的 sha256 作为版本号，不一致 → 409；B. 后写覆盖 | **A**：防止覆盖别人刚做的手工修改 |
| 5 | 管理面 MCP 是否提供写配置的工具 | A. 不提供；B. 提供 | **A**：AI 改配置走「编辑文件 + `gateway reload --dry-run`」，改动可以审阅；在 MCP 里一步写入不便审阅 |
| 6 | 删除池里还有 Key | A. 允许，Key 成为孤立 Key（可恢复）；B. 必须先删 Key | **A**：与现有的「配置里去掉池」行为一致，且可以恢复 |
