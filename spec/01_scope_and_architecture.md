# 01 范围与架构（Scope & Architecture）

## 1. 产品定义

Universal AI Gateway 是一个**单进程、单静态二进制**的大模型 API 网关：

- 下游客户端用自己熟悉的协议（Anthropic Messages 或 OpenAI Chat Completions）调用网关；
- 网关鉴权、限流、计费，从上游 Key 池中选一个 Key，把请求转发给上游；
- 同一会话尽量固定在同一个上游 Key 上，以保住上游的 Prompt Cache；
- 上游 Key 失效或限流时自动换 Key，并且不会给客户端返回拼接错乱的数据。

## 2. 里程碑与范围

### M1：透传 MVP（必须先完成）

| 功能 | 说明 |
|:--|:--|
| 入站协议 | `POST /v1/messages`（Anthropic）、`POST /v1/chat/completions`（OpenAI）、`GET /v1/models` |
| 上游协议 | `anthropic`（Anthropic 官方 API）、`openai_chat`（OpenAI 及兼容接口：DeepSeek、Moonshot、Ollama 等） |
| 路由 | **只支持同协议透传**：入站 `anthropic` → `anthropic` 池；入站 `openai_chat` → `openai_chat` 池。协议不同时返回 `400 invalid_request`（M2 才支持转译） |
| 流式 | 支持 SSE 流式与非流式 |
| 鉴权 | 客户端 Token（`uai_live_`），HMAC 索引，内存查找 |
| 限流 | 每客户端 RPM 令牌桶 |
| 计费 | 预留-结算账本，计费事件写入 SQLite `usage_events` |
| 调度 | Key 池、状态机、冷却、熔断、会话亲和、首字节前重试 |
| 管理 | 独立监听的管理 API（JSON），无 Web UI |
| 审计 | 请求元数据审计日志（不存 Prompt 正文），可丢弃，按天数清理 |
| 存储 | 嵌入式 SQLite（`modernc.org/sqlite`，纯 Go） |

### M2：跨协议转译

OpenAI 入站 → Anthropic 上游、Anthropic 入站 → OpenAI 上游（请求、非流式响应、流式响应），见 `10_ir_translation.md`。

### M3：运维增强

Prometheus 指标端点、Gemini 协议、WASM 插件、管理 Web UI。M3 的规格在 M2 完成后补充，**现在不要实现**。

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
  provider/             上游协议适配器接口
  provider/anthropic/   Anthropic 适配器
  provider/openai/      OpenAI Chat 适配器
  proxy/                数据面编排：一次模型请求的完整流程
  httpapi/              数据面 HTTP 路由、中间件、协议错误格式
  admin/                管理面 HTTP API
  ir/                   （M2）规范化中间表示类型
  translate/            （M2）IR 与各协议互转
  archtest/             分层依赖规则测试
tools/check/            一键检查程序（go run ./tools/check）
```

## 4. 分层规则（由 `internal/archtest` 自动检查）

依赖只能**从上往下**，同层之间不得互相依赖，除非下表明确允许。

```
第 5 层  cmd/gateway                                   （可以依赖任何 internal 包）
第 4 层  httpapi   admin
第 3 层  proxy
第 2 层  ledger  audit  scheduler  auth  ratelimit  provider/*  translate  store/sqlite
第 1 层  provider  store  pricing  sse  jsonx  crypto  ir  config  obs  clientip
第 0 层  apperr
```

明确允许的同层依赖（只有这些）：`store → crypto`；`provider → sse、pricing、jsonx`；`ir → pricing`。

第三方模块只能被指定的包导入：`gopkg.in/yaml.v3` 只在 `config`；`modernc.org/sqlite` 只在 `store/sqlite`。

新增任何包都必须先在 `internal/archtest/archtest_test.go` 的 `layers` 表中登记——这需要架构负责人批准，实现任务中不要自己加。

额外规则（违反即测试失败）：

| 编号 | 规则 |
|:--|:--|
| ARCH-01 | `proxy`、`httpapi` **禁止**导入 `database/sql`、`internal/store/sqlite`（数据面不碰数据库） |
| ARCH-02 | 只有 `cmd/gateway` 可以导入 `internal/store/sqlite`；其他包通过 `internal/store` 接口访问存储 |
| ARCH-03 | `provider/anthropic` 与 `provider/openai` 互不导入 |
| ARCH-04 | 第 1 层包**禁止**导入任何第 2 层及以上的包 |
| ARCH-05 | 任何包**禁止**导入 `math/rand`（测试文件除外，测试中如需伪随机用 `math/rand/v2` 并固定种子） |
| ARCH-06 | `admin` 可以导入 `store`、`auth`、`crypto`、`ledger`、`scheduler`，但**禁止**导入 `proxy` |

## 5. 运行时结构

```
                    ┌──────────────────────── 进程 ─────────────────────────┐
 客户端 ──HTTP──▶   │ 数据面监听 :8080 (httpapi)                              │
                    │   中间件：RequestID → Recover → BodyLimit → Auth → RPM │
                    │   └─▶ proxy.Handler                                     │
                    │        ├─ ledger.Reserve / Settle（内存）               │
                    │        ├─ scheduler.Acquire / Report（内存）            │
                    │        ├─ provider.Adapter（构造上游请求、提取 usage）  │
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

以下步骤是 `proxy` 与 `httpapi` 的实现蓝本，**顺序不可调换**。编号 `LIFE-xx` 在测试中引用。

| 步骤 | 编号 | 动作 | 失败时 |
|:-:|:--|:--|:--|
| 1 | LIFE-01 | 路由按路径确定入站协议（不做内容嗅探） | 404 |
| 2 | LIFE-02 | 分配 `request_id`（`req_` + 24 位 base32 随机），写入响应头 `X-Request-Id` | — |
| 2a | LIFE-02a | 解析真实客户端 IP（可信代理，spec/03 §7）；按 IP 限流；占用全局并发与请求体预算（spec/09 §5） | 429 `rate_limited` / 503 `overloaded` |
| 4 | LIFE-04 | 提取凭据（`Authorization: Bearer` → `x-api-key` → `x-goog-api-key`），HMAC 后查内存索引 | 401 `unauthenticated` |
| 5 | LIFE-05 | 客户端被禁用 | 403 `permission_denied` |
| 6 | LIFE-06 | RPM 令牌桶取 1 个令牌 | 429 `rate_limited`，带 `Retry-After` |
| 6a | LIFE-03 | （在 `proxy` 中）读取请求体，上限 `limits.max_request_body_bytes` | 413 `request_too_large` |
| 7 | LIFE-07 | 解析请求体中的 `model` 与 `stream` 字段；按 `models` 配置解析出模型路由；检查客户端模型白名单 | 400 / 403 |
| 8 | LIFE-08 | 入站协议 ≠ 路由的上游协议 → M1 返回 400 | 400 `invalid_request` |
| 9 | LIFE-09 | 估算预留金额，`ledger.Reserve` | 402 `insufficient_quota` / 503 `overloaded` |
| 10 | LIFE-10 | 计算亲和键 | — |
| 11 | LIFE-11 | 尝试循环（最多 `max_attempts` 次）：`scheduler.Acquire` → 构造上游请求 → 发送 → 读响应头 | 见 07 §6 |
| 12 | LIFE-12 | 上游返回 2xx：写响应头，开始向客户端转发（**从此刻起不得重试**） | 流中错误按 08 §6 处理 |
| 13 | LIFE-13 | 转发过程中旁路提取 usage | 提取失败按 06 §5 估算 |
| 14 | LIFE-14 | `defer`：释放 Key 租约、`scheduler.Report` 结果 | — |
| 15 | LIFE-15 | `defer`：`ledger.Settle`（有用量）或 `ledger.Release`（确定未产生上游用量） | — |
| 16 | LIFE-16 | `defer`：`audit.Submit` 审计记录 | 队列满则丢弃并计数 |

## 7. 包级依赖注入约定

- 每个有状态的组件提供 `New(...)` 构造函数，依赖通过参数传入，**禁止**包级全局变量保存状态（常量与无状态的 `var` 表除外）。
- 时间通过 `func() time.Time` 注入（字段名 `Now`），测试中替换为固定时钟。
- 需要后台运行的组件提供 `Run(ctx context.Context) error`：阻塞运行直到 `ctx` 取消，退出前完成最后一次刷盘，然后返回 `nil`。
- 所有组件在 `cmd/gateway/main.go` 中装配；装配顺序见 `03_config.md` §5。
