# T66 声明式 API 能力 A：业务错误、用量、异步任务（M4，规格草案）

- **类型**：规格 + 实现。2026-11-13 用户批准 §7 的全部建议（「全部按建议」）。
- **前置**：建议 T61 先做。总览见 [M4_generalization.md](./M4_generalization.md)。
- **约束**：只改 `rest` 适配器、配置与装配代码。不改内核与契约的导出 API；不改分层规则与 adaptertest 套件；不加依赖。没有 `api` 声明的服务行为与现在完全相同。

## 1. 要解决的问题

通用 REST 转发（spec/08 §8.3）现在只做「原样转发 + 按次计费」，下面三种很常见的情况都处理不了，只能写专用适配器：

| 情况 | 例子 | 现在的后果 |
|:--|:--|:--|
| 业务错误放在 HTTP 200 的响应体里 | MinerU `{"code":-60018}`（日额度用完）、`A0202`（Token 错误） | 当成成功：不换 Key、不冷却，还按成功计费 |
| 用量在响应或请求里，不是「每次 1 个」 | Tavily `usage.credits`、`search_depth=advanced` 记 2 个；MinerU 按页数 | 只能按次计费 |
| 异步任务只能在创建它的账号下查询 | MinerU `POST /api/v4/extract/task` → `GET /api/v4/extract/task/{id}` | 查询落到别的 Key，返回 -60012 |

## 2. 配置

声明放在服务上（`models[].api`），与已有的 `models[].tools` 并列，只对 `rest` 池的服务有效：

```yaml
models:
  - name: mineru-api
    pool: mineru-rest                  # protocol: rest, base_url: https://mineru.net
    price: { pages: "0.01", requests: "0" }
    reserve: { pages: 600 }
    api:
      errors:                          # 按顺序匹配，第一条命中的生效；2xx 与非 2xx 响应都检查
        - when: { json: msgCode, in: ["A0202", "A0211"] }
          outcome: auth_failed
        - when: { json: code, in: [-60018, -60019] }
          outcome: rate_limited
          cooldown_until: { time: "00:01", tz: "+08:00" }   # 冷却到下一个北京时间 00:01
        - when: { json: code, in: [-60009, -60007, -10001] }
          outcome: overloaded
        - when: { json: code, in: [-60012, -60013] }
          outcome: client_error
          status: 404
        - when: { json: code, not_in: [0] }
          outcome: client_error
          status: 400
      endpoints:                       # 按顺序匹配「方法 + 路径模板」，第一条命中的生效
        - match: "POST /api/v4/extract/task"
          task_id: data.task_id        # 成功时记录任务 ID，固定到本次所用的 Key
          usage: { pages: { request_query: pages, default: 600 } }   # 提交时还不知道页数：按客户端给的 ?pages= 或缺省值
        - match: "GET /api/v4/extract/task/{task}"
          pinned_by: task              # 用路径中的 {task} 查固定记录：只用创建任务的 Key，不换 Key
          usage: {}                    # 不计费
```

```yaml
  - name: tavily-api
    pool: tavily-rest                  # base_url: https://api.tavily.com
    price: { credits: "0.008" }
    reserve: { credits: 2 }
    api:
      errors:
        - when: { status: [432, 433] }  # 示例：额度用完的状态码与重置时间以官方文档为准
          outcome: rate_limited
          cooldown_until: { day_of_month: 1, time: "00:00", tz: "UTC" }
      endpoints:
        - match: "POST /search"
          usage:
            credits:
              json: usage.credits              # 上游报告了就用上游的
              request_json: search_depth       # 否则按请求推算
              map: { advanced: 2 }
              default: 1
```

### 2.1 规则

| 编号 | 规则 |
|:--|:--|
| DA-01 | `api` 只允许出现在 `rest` 池的服务上，其他协议的服务出现时配置校验失败。没有 `api` 的服务行为与 T51 / T54 完全相同 |
| DA-02 | 字段路径只支持点分隔的对象键与数组下标（`data.task_id`、`items.0.id`），不支持通配、过滤、表达式。不存在、类型不符都视为「没有这个字段」 |
| DA-03 | `when` 可以写 `status`（状态码列表）、`json` 加 `in` / `not_in` / `exists`（值可以是字符串或数字，按 JSON 原值比较，`1` 与 `"1"` 不相等）。同一条里的多个条件全部满足才算命中。响应体不是 JSON 时，所有 `json` 条件都不命中 |
| DA-04 | `outcome` 取 `rate_limited`、`auth_failed`、`permission_denied`、`overloaded`、`server_error`、`client_error`，含义、换 Key、冷却与隔离沿用 spec/07 §5（`permission_denied` 照样受 RS-16 约束，不隔离 Key）。只有 `client_error` 会把上游错误体连同 `status`（缺省 400，只能用于 client_error）交给客户端；其他类别由内核在重试结束后输出统一错误（实现时按契约修正：`AttemptError.Body` 只在 `OutcomeClientError` 时转发） |
| DA-05 | `cooldown_until` 计算到下一个满足条件的时刻：只写 `time` 表示每天，加 `day_of_month` 表示每月；`tz` 为 `±HH:MM` 或 `UTC`。结果作为 `RetryAfter` 交给调度器，仍受池的 `cooldown.max` 限制。没有 `cooldown_until` 时沿用上游的 `Retry-After` 或池的缺省冷却 |
| DA-06 | 2xx 且没有命中错误规则才算成功。成功时按命中的 endpoint 提取用量；没有命中任何 endpoint 时，用量为 `{requests: 1}`（与现在相同） |
| DA-07 | 用量的每个单位按顺序取值：`json`（响应字段）→ `request_json`（请求体字段）或 `request_query`（查询参数），经 `map` 换算，没有 `map` 时取原值 → `default`。来自响应时 `UsageUpstream`，来自请求或缺省值时 `UsageEstimated`；全都没有时该单位不报告；一个单位都没有时 `UsageNone`（按预留结算）。值必须是非负整数，否则视为没有并记一次 Warn（不含字段值）。`usage: {}` 表示确定不计费 |
| DA-07a | 异步任务在提交时计费（与 T59 相同）：完成后的真实用量要跨请求记住任务状态，A 阶段不做。需要精确计费的，用 T67 的同步等待（能拿到真实页数），或由客户端在提交时给出 `?pages=` |
| DA-08 | `task_id`：成功响应中取到任务 ID 时放进 `Response.Pins`，记录值为 `<服务名>:<任务 ID>`，避免不同服务的任务 ID 冲突 |
| DA-09 | `pinned_by: <占位符>`：入站解析时按路径模板取出占位符的值，设置严格亲和（`Strict`，值同 DA-08）。未命中固定记录 → 404 `not_found`（spec/07 SCH-17），绝不换 Key。只对 `/rest/` 下的直接请求与工具调用生效 |
| DA-10 | 路径模板的占位符只匹配一个路径段，按段解码后比较；不支持 `{path...}` |
| DA-11 | 需要检查响应体的请求（有 JSON 错误规则或用量规则），2xx 响应按 `MaxBufferedBytes` 缓冲读取后再转发（`ResponseBuffered`）；超过上限时返回 502，按 `local_error` 处理，不惩罚 Key。其他响应仍是字节流 |
| DA-12 | 换 Key 重试只发生在请求体已缓冲的路由上（`GET`、`DELETE`、`PUT`、`/rest-idem/`，与 RS-19 相同）。`/rest/` 下的 `POST` 是流式请求体，命中 `rate_limited` 也只尝试一次，Key 照常冷却；需要自动换 Key 的接口请用 `/rest-idem/` 或在工具声明里写 `idempotent: true` |
| DA-13 | 计量单位：`rest` 的 `Units()` 是所有服务 `usage` 中出现的单位加上 `requests`。每个服务必须给自己会报告的单位定价；其他单位装配时自动补 `"0"`（服务永远不会报告它们，见决策 2） |
| DA-14 | 日志与错误信息中不出现响应体内容、请求体内容与任务 ID 以外的字段值；任务 ID 只写进固定记录（内存） |

## 3. 实现要点

- 配置：`internal/config` 增加 `ModelConfig.API` 及其校验（DA-01 ~ DA-05、DA-10 的静态部分：outcome 名、状态码范围、时区、路径模板语法、至少一个条件）。配置包不认识协议名，「只对 rest 生效」的检查放在装配代码（`cmd/gateway`），与 T54 的工具声明相同。
- 适配器：`rest.Options` 增加 `APIs map[string]APIDef`（键为服务名），沿用 `RegisterWithOptions`。入站按 DA-09 设置亲和；出站在 `Prepare` 中按服务名找到声明放进 `Call.State`，`Do` 中执行错误规则与用量提取。服务名来自 `Envelope.Model`，内核不需要知道这些声明。
- 热重载：声明随配置热重载（与 `tools` 相同，T55）。
- 契约：用到的都是已有机制，包括 `AttemptError`、`Response.Pins`、`AffinityHint.Strict`、`Meter`、`Outcome.RetryAfter`。如果实现中发现不够用，停下来报告。

## 4. 测试

- 字段路径（对象、数组下标、不存在、类型不符）；`when` 的每种条件与组合；规则顺序。
- 错误规则：2xx 业务错误 → 各类 outcome、状态码、错误体透传、Key 状态（冷却、隔离、不惩罚）；`cooldown_until` 跨日、跨月、时区、受 `cooldown.max` 限制。
- 用量：响应字段、请求字段加 `map`、`default`、非整数、负数、`usage: {}`、未命中 endpoint 时为 `{requests: 1}`；`UsageUpstream` / `UsageEstimated` / `UsageNone` 的判定。
- 异步任务：创建后固定；查询只用同一 Key；未命中返回 404；不同服务的同名任务 ID 互不干扰。
- 缓冲上限；流式 POST 不重试；`/rest-idem/` 换 Key 重试。
- 配置校验：非 rest 服务带 `api`、未定价单位、非法时区、非法模板、空条件。
- adaptertest 一致性套件照常通过（不修改套件）。

## 5. 实测（验收）

用真实 Key，经同一个网关、全部只用配置：

1. **MinerU 异步任务**：`POST /rest-idem/mineru-api/api/v4/extract/task?pages=13` 提交，`GET /rest/mineru-api/api/v4/extract/task/{id}` 轮询直到完成，查询始终落在创建任务的 Key 上（87 把 Key 的池里不再出现 -60012）；Token 错误（造一把假 Key）被隔离；计费为 13 页，查询不计费。
2. **Tavily 搜索**：`basic` 与 `advanced` 各一次，计费与专用适配器 `tavily` 一致。
3. 没有 `api` 声明的服务（Bivia）行为不变。
4. 日志中没有 Key、响应内容与账号信息。

### 5.1 实测结果（2026-11-13）

全部只用配置，经通用 `rest` 适配器，真实 Key：MinerU 87 把、Tavily 86 把（rest 池与专用 `tavily` 池各一份）。

- [x] MinerU：提交 2 个任务，轮询 9 次（pending 3、running 4、done 2），**每次都落在创建任务的 Key 上**，没有出现 -60012 或 404；从未创建的任务 ID 直接返回 404（`unknown or expired task; resubmit`），不尝试其他 Key；计费 26 页（2 × 13），查询不计费；87 把 Key 全部保持可用。
- [x] 假 Token：MinerU 对错误 Token 返回 **HTTP 401** 加 `msgCode: A0202`（不是 HTTP 200 里的 `code`），第一次 401 按 ST-06 冷却 60 秒，第二次 401 隔离（`upstream 401 twice`）。示例配置据此改为匹配 `msgCode`（401 本来就会被分类为认证失败，这条规则只是写明含义）。
- [x] Tavily：rest 服务 `basic` 计 1、`advanced` 计 2，与专用适配器（`basic` 1 + `advanced` 2 = 3）一致；带 `include_usage: true` 时上游返回 `usage` 字段，按上游值计费。
- [x] 导入输出与网关日志中没有 Key、Token 或账号信息。
- [ ] 第 3 项（Bivia）没有实测，由已有的 rest 端到端测试与 `TestDeclaredPassThroughAndLimits` 中「没有声明的服务」用例覆盖。
- 业务错误放在 HTTP 200 响应体里的情况（MinerU -60018 日额度、-60012）无法用真实账号主动触发，由端到端测试 `TestE2E_REST_DeclaredAPI`（假上游：3 把 Key、两把日额度用完后换到第三把并冷却到次日、43 页计费、价格自动补 0）覆盖。

### 5.2 已知限制

- 异步任务只在提交时计费（DA-07a），精确页数要等 T67 的同步等待。
- 固定记录只在内存里，网关重启后任务查询返回 404（T68）。
- `/rest/` 下的 `POST` 不换 Key、读不到请求体字段（DA-12），要用 `/rest-idem/`。
- 错误规则只能看状态码与响应体字段，不能看响应头。

## 6. 改动文件

- `internal/config`（类型、校验、测试）；`gateway.example.yaml`。
- `internal/adapter/rest`：新增 `api.go`（声明的执行：字段路径、错误规则、用量、模板匹配）与测试；`ingress.go`（DA-09）、`egress.go`（DA-06、DA-11）、`register.go`（`Units()`、`Options`）。
- `cmd/gateway`：新增 `rest_api.go`（声明转换、DA-01、DA-13 价格补 0）与 `e2e_rest_api_test.go`；`adapters.go`、`reload.go`（内核使用补齐价格后的配置副本）。
- `spec/03`（配置）、`spec/08` §8.3.6（新增 DA 规则）、`docs/DEPLOYMENT.md` §9、`.claude/skills/add-api/SKILL.md`（教 AI 根据官方文档写 `api` 声明）。

## 7. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 声明放在哪里 | A. 服务上（`models[].api`）；B. 池上（`pools[].api`） | **A**：同一个上游的不同服务（例如只读与读写）可能要不同的计费；与 `tools` 放在一起，AI 写配置时只看一处 |
| 2 | 计量单位的定价 | A. 每个服务只为自己会报告的单位定价，其他单位装配时自动补 `"0"`；B. 每个 rest 服务必须为所有 rest 服务用到的单位定价 | **A**：B 会让加一个服务就要改所有服务的价目表。A 是安全的，因为服务永远不会报告别的服务声明的单位 |
| 3 | 字段路径的表达能力 | A. 只支持点路径与数组下标；B. 完整 JSONPath | **A**：已有的 API 都够用，不需要新依赖，也不容易写出意外匹配 |
| 4 | 流式 POST 遇到额度用完时 | A. 只尝试一次，客户端想自动换 Key 就走 `/rest-idem/`；B. 有 `api` 声明的服务默认缓冲 POST 请求体以便换 Key | **A**：不改变已有路由的语义；工具调用可以用 `idempotent: true`。B 会让大文件上传进内存 |
| 5 | 专用适配器 `mineru`、`tavily` | A. 保留；B. T67 完成后改为配置并删除 | **A**：先保留，声明式能力稳定后再单独评估 |
