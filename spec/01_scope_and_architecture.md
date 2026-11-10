# 01 范围与架构（Scope & Architecture）

## 1. 产品定义

Universal AI Gateway 是一个**单进程、单静态二进制**的 AI API 网关。它代理的不只是大模型：
任何「用 API Key 调用、按用量计费」的 HTTP API（搜索、文档解析、语音、图像……）都可以通过适配器接入（spec/08 §8）。

- 下游客户端用自己熟悉的协议（Anthropic Messages、OpenAI Chat Completions、OpenAI Responses，或任一已接入 API 的原生协议）调用网关；
- AI 智能体还可以通过 **MCP**（`/mcp` 或 `gateway mcp`）与**命令行**（`gateway call`）把网关的服务当作工具调用（spec/14）；
- 网关鉴权、限流、按计量单位计费，从上游 Key 池中选一个 Key，把请求转发给上游；
- 同一会话尽量固定在同一个上游 Key 上，以保住上游的 Prompt Cache；
- 上游 Key 失效或限流时自动换 Key，并且不会给客户端返回拼接错乱的数据。

## 2. 里程碑与范围

### M1：透传 MVP（必须先完成）

| 功能 | 说明 |
|:--|:--|
| 入站协议 | `POST /v1/messages`（Anthropic）、`POST /v1/chat/completions`（OpenAI Chat）、`/v1/responses*`（OpenAI Responses，T09C）、`GET /v1/models`；非大模型 API 的适配器见「扩展适配器」任务卡 T30、T31 |
| 供 AI 调用 | MCP 服务端（Streamable HTTP `/mcp` + stdio 桥 `gateway mcp`）、命令行 `gateway tools` / `gateway call`（T32、T33，spec/14） |
| 上游协议 | `anthropic`（Anthropic 官方 API）、`openai_chat`（OpenAI 及兼容接口：DeepSeek、Moonshot、Ollama 等）、`openai_responses`（OpenAI / Azure OpenAI 的 Responses API） |
| 路由 | **只支持同协议透传**：入站 `anthropic` → `anthropic` 池；入站 `openai_chat` → `openai_chat` 池。协议不同时返回 `400 invalid_request`（M2 才支持转译） |
| 请求与响应形态 | 请求体：缓冲 / 流式上传 / 无；响应：缓冲体 / SSE 事件流 / 原样字节流（spec/13 §4）；异步任务固定到创建它的 Key（spec/07 §7.1） |
| 鉴权 | 客户端 Token（`uai_live_`），HMAC 索引，内存查找 |
| 限流 | 每客户端 RPM 令牌桶 |
| 计费 | 按任意计量单位（token、次数、页数……）的预留-结算账本，计费事件写入 SQLite `usage_events`（spec/06） |
| 调度 | Key 池、状态机、冷却、熔断、会话亲和、首字节前重试 |
| 管理 | 独立监听的管理 API（JSON），无 Web UI |
| 审计 | 请求元数据审计日志（不存 Prompt 正文），可丢弃，按天数清理 |
| 存储 | 嵌入式 SQLite（`modernc.org/sqlite`，纯 Go） |

### M2：跨协议转译

OpenAI 入站 → Anthropic 上游、Anthropic 入站 → OpenAI 上游（请求、非流式响应、流式响应），见 `10_ir_translation.md`。

### M3：运维增强与协议扩展

Prometheus 指标端点（`GET /admin/metrics`，详见 `15_metrics.md`，T45 已完成）、Gemini 协议（同协议透传见 `08_adapters.md` §10 / T47，跨协议转译见 `10_ir_translation.md` §11 / T48）、管理 Web UI（T49）、WASM 插件。

Gemini 协议范围：
- 支持 Google AI Studio 的 Gemini API（`generativelanguage.googleapis.com`）的 `generateContent`、`streamGenerateContent`（需 `alt=sse`）、`countTokens` 以及模型目录 `GET /v1beta/models`。
- 明确不做：Vertex AI（OAuth/服务账号认证及独立端点）、`embedContent`、`batchEmbedContents`、文件上传（Files API）、`cachedContents` 管理 API、Live API（WebSocket）及 Batch API。

### 明确不做（任何里程碑都不做）

- 订阅账号（Claude Pro/Max、ChatGPT Plus）逆向、TLS 指纹伪造、住宅代理防封（违反服务条款，见 `06_architecture_review_and_revisions.md` ADR-11）。
- 多实例 / 集群部署（v1 严格单实例，启动时锁定数据目录）。
- 存储 Prompt / 响应正文。

## 3. 模块地图

```
cmd/gateway/            进程入口：解析参数、加载配置、装配依赖、启动/关闭（组装层）
internal/
  apperr/               统一错误类型与错误码（所有包都可以用）
  config/               配置结构、加载、默认值、校验
  obs/                  日志初始化、凭据脱敏
  clientip/             按可信代理解析真实客户端 IP
  crypto/               主密钥、HKDF 派生、信封加密
  jsonx/                JSON 顶层字段局部改写工具
  sse/                  SSE 事件读取器与写入器
  pricing/              价格与成本计算（纯函数）
  store/                持久化接口与记录类型（无实现）
  store/sqlite/         store 接口的 SQLite 实现、迁移脚本
  auth/                 Token 生成/哈希、凭据提取、内存索引
  ratelimit/            每客户端 RPM 令牌桶
  ledger/               预留-结算账本、计费事件刷盘 worker
  audit/                审计日志异步流水线
  scheduler/            Key 池、状态机、亲和缓存、选择策略
  protocol/             【冻结契约】入站/出站适配器、转换器接口，Envelope，注册表，上游错误分类
  adapter/anthropic/    Anthropic 协议的入站 + 出站适配器
  adapter/openai/       OpenAI Chat 协议的入站 + 出站适配器（出站兼容 DeepSeek/Moonshot/Ollama）
  adapter/openairesp/   OpenAI Responses 协议的入站 + 出站适配器，及工具 ask_model
  adapter/adaptertest/  适配器一致性测试套件（只供测试导入）
  core/                 【内核】协议无关的请求编排：准入 → 路由 → 尝试 → 转发 → 结算
  httpapi/              数据面 HTTP 路由、中间件、协议错误格式
  admin/                管理面 HTTP API
  mcp/                  供 AI 调用：MCP 服务端（/mcp）、MCP 客户端与 stdio 桥（gateway mcp / tools / call）
  metrics/              （M3）Prometheus 格式指标收集与导出（15_metrics.md）
  ir/                   （M2）规范化中间表示类型
  translate/            （M2）IR 与各协议互转
  archtest/             分层依赖规则测试
tools/check/            一键检查程序（go run ./tools/check）
```

## 4. 分层规则（由 `internal/archtest` 自动检查）

依赖只能**从上往下**，同层之间不得互相依赖，除非下表明确允许。

```
第 5 层  cmd/gateway                                   （可以依赖任何 internal 包）
第 4 层  httpapi   admin   mcp
第 3 层  core（内核）
第 2 层  ledger  audit  scheduler  metrics  auth  ratelimit  adapter/*  translate  store/sqlite
第 1 层  protocol  store  pricing  sse  jsonx  crypto  ir  config  obs  clientip
第 0 层  apperr
```

明确允许的同层依赖（只有这些）：`store → crypto`；`config → pricing`；`protocol → sse、pricing`；`ir → pricing`。`httpapi` 与 `mcp` 同层、互不导入：`/mcp` 的处理器由 `cmd/gateway` 注入 `httpapi.DataDeps`（RTE-05）。

第三方模块只能被指定的包导入：`gopkg.in/yaml.v3` 只在 `config`；`modernc.org/sqlite` 只在 `store/sqlite`。

新增任何包都必须先在 `internal/archtest/archtest_test.go` 的 `layers` 表中登记——这需要架构负责人批准，实现任务中不要自己加。

额外规则（违反即测试失败）：

| 编号 | 规则 |
|:--|:--|
| ARCH-01 | `core`、`httpapi` **禁止**导入 `database/sql`、`internal/store/sqlite`（数据面不碰数据库） |
| ARCH-02 | 只有 `cmd/gateway` 可以导入 `internal/store/sqlite`；其他包通过 `internal/store` 接口访问存储 |
| ARCH-03 | `adapter/*` 之间互不导入 |
| ARCH-04 | 第 1 层包**禁止**导入任何第 2 层及以上的包 |
| ARCH-05 | 任何包**禁止**导入 `math/rand`（测试文件除外，测试中如需伪随机用 `math/rand/v2` 并固定种子） |
| ARCH-06 | `admin` 可以导入 `store`、`auth`、`crypto`、`ledger`、`scheduler`，但**禁止**导入 `core` |
| ARCH-07 | 具体适配器包 `internal/adapter/<name>` **只能**被 `cmd/gateway`（`adapters.go`）导入；内核、路由、管理面一律通过 `protocol` 接口使用适配器。注意 core（第 3 层）导入 adapter（第 2 层）在分层上是「向下」的，所以必须有这条专门规则 |
| ARCH-08 | `internal/adapter/adaptertest` 只能被 `_test.go` 文件导入 |

## 5. 运行时结构

```
                    ┌──────────────────────── 进程 ─────────────────────────┐
 客户端 ──HTTP──▶   │ 数据面监听 :8080 (httpapi)                              │
                    │   中间件：RequestID → Recover → BodyLimit → Auth → RPM │
                    │   └─▶ core.Handler(入站适配器)                          │
                    │        ├─ ledger.Reserve / Settle（内存）               │
                    │        ├─ scheduler.Acquire / Report（内存）            │
                    │        ├─ protocol.Egress（构造上游请求、提取 usage）   │
                    │        └─ audit.Submit（非阻塞）                        │
                    │                                                         │
 管理员 ──HTTP──▶   │ 管理面监听 127.0.0.1:8081 (admin) ──▶ store（SQLite）   │
                    │                                                         │
                    │ 后台 worker：ledger.Flusher、audit.Writer、             │
                    │              auth 索引重载、audit 过期清理              │
                    └─────────────────────────────────────────────────────────┘
```

**状态归属**：
- 客户端索引（Token → Principal）：内存快照，由控制面在启动和管理变更后整体替换（copy-on-write）。
- 余额与在途预留：内存（`ledger`），持久化来源是 `usage_events` 表。
- 上游 Key 明文：启动时解密后常驻内存（`scheduler.Key.Secret`）；数据库中只有密文。
- 亲和缓存、冷却、熔断状态：仅内存，重启丢失（可接受）。

## 6. 一次请求的完整生命周期（M1）

以下步骤是 `core` 与 `httpapi` 的实现蓝本（内核部分的细则见 spec/13），**顺序不可调换**。编号 `LIFE-xx` 在测试中引用。

| 步骤 | 编号 | 动作 | 失败时 |
|:-:|:--|:--|:--|
| 1 | LIFE-01 | 路由按路径确定入站协议（不做内容嗅探） | 404 |
| 2 | LIFE-02 | 分配 `request_id`（`req_` + 24 位 base32 随机），写入响应头 `X-Request-Id` | — |
| 2a | LIFE-02a | 解析真实客户端 IP（可信代理，spec/03 §7）；按 IP 限流；占用全局并发与请求体预算（spec/09 §5） | 429 `rate_limited` / 503 `overloaded` |
| 4 | LIFE-04 | 提取凭据（`Authorization: Bearer` → `x-api-key` → `x-goog-api-key`），HMAC 后查内存索引 | 401 `unauthenticated` |
| 5 | LIFE-05 | 客户端被禁用 | 403 `permission_denied` |
| 6 | LIFE-06 | RPM 令牌桶取 1 个令牌 | 429 `rate_limited`，带 `Retry-After` |
| 6a | LIFE-03 | （在 `core` 中）按路由的请求体模式读取请求体（缓冲 / 流式 / 无），上限见 CORE-06 | 413 `request_too_large` |
| 7 | LIFE-07 | 入站适配器 `Parse` 得到 `Envelope`（a）；按 `Envelope.Model` 解析服务路由并检查白名单（b） | 400 / 403 |
| 8 | LIFE-08 | 生成上游调用 `Call`：同协议 `Egress.Prepare`，否则查找转换器，找不到返回 400（spec/13 §3） | 400 `invalid_request` |
| 9 | LIFE-09 | 按 `Envelope.Estimate` 与模型 `reserve` 估算预留金额，`ledger.Reserve` | 402 `insufficient_quota` / 503 `overloaded` |
| 10 | LIFE-10 | 计算亲和键 | — |
| 11 | LIFE-11 | 尝试循环（最多 `max_attempts` 次；流式上传只 1 次）：`scheduler.Acquire` → `Egress.Do`（一次尝试可含多次上游调用） | 见 07 §6 |
| 12 | LIFE-12 | 得到可转发的响应：写响应头，按 `Response.Kind` 开始转发（**从此刻起不得重试**） | 流中错误按 13 §4.4 处理 |
| 13 | LIFE-13 | 转发结束后从 `Response.Meter` 取用量；记录任务固定（`Response.Pins`） | 无用量时按 13 §2.1 结算 |
| 14 | LIFE-14 | `defer`：释放 Key 租约、`scheduler.Report` 结果 | — |
| 15 | LIFE-15 | `defer`：`ledger.Settle`（有用量）或 `ledger.Release`（确定未产生上游用量） | — |
| 16 | LIFE-16 | `defer`：`audit.Submit` 审计记录 | 队列满则丢弃并计数 |

## 7. 包级依赖注入约定

- 每个有状态的组件提供 `New(...)` 构造函数，依赖通过参数传入，**禁止**包级全局变量保存状态（常量与无状态的 `var` 表除外）。
- 时间通过 `func() time.Time` 注入（字段名 `Now`），测试中替换为固定时钟。
- 需要后台运行的组件提供 `Run(ctx context.Context) error`：阻塞运行直到 `ctx` 取消，退出前完成最后一次刷盘，然后返回 `nil`。
- 所有组件在 `cmd/gateway/main.go` 中装配；装配顺序见 `03_config.md` §5。

## 8. 微内核原则（扩展性的核心约定）

```
        会变、会增加                      稳定、尽量不变                      会变、会增加
┌──────────────────────┐        ┌────────────────────────────┐        ┌──────────────────────┐
│ 入站适配器 Ingress   │        │ 内核 core                  │        │ 出站适配器 Egress    │
│ anthropic / openai / │──────▶│ 鉴权后的准入、路由、预留、  │──Call─▶│ anthropic / openai / │
│ tavily / mineru ……   │Envelope│ 选 Key、重试、转发、结算    │◀─Resp──│ tavily / mineru ……   │
└──────────────────────┘        └────────────────────────────┘        └──────────────────────┘
            └──────────── 契约：internal/protocol（冻结）────────────────────┘
```

1. **扩展只发生在两端**：新增客户端协议 = 新增 Ingress；新增上游厂商 = 新增 Egress（OpenAI 兼容厂商只需配置）；跨协议 = 新增 Translator。步骤见 spec/08 §6。
2. **内核协议无关、不假设大模型**：内核不认识任何协议名和请求体字段，也没有 token 专用逻辑；计费只认计量单位，转发只认响应形态（spec/13 §1）。
3. **契约冻结**：`internal/protocol` 与内核导出 API 的修改需要 ADR 批准（spec/13 §7）。
4. **质量靠套件保证**：所有适配器必须通过同一套一致性测试（spec/08 §5）。
5. **由机器守护**：archtest 的 ARCH-07 / ARCH-08 保证没有人能让内核依赖具体协议。
