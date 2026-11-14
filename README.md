# Universal AI Gateway 技术全景与架构设计文档集

> **项目代号**：Project Titan | **核心语言**：Pure Go 1.24+ (Zero CGO) | **运行环境**：Cross-Platform (Linux / Windows / macOS)

> **开发者（包括 AI 编码助手）请从这里开始**：
> 1. [`AGENTS.md`](./AGENTS.md) —— 工作守则、禁止事项、完成的定义；
> 2. [`spec/README.md`](./spec/README.md) —— 实现规格（唯一实现依据）；
> 3. [`spec/11_tasks.md`](./spec/11_tasks.md) —— 任务卡，一次领一张。
>
> 一键检查：`go run ./tools/check`。本页与 `00`–`06` 白皮书只作设计背景，与 `spec/` 冲突时以 `spec/` 为准。

---

## 1. 项目总览与定位

**Universal AI Gateway** 是专为现代化 AI Agent、大语言模型应用和多模型异构基础设施设计的高性能、企业级网关与智能路由中枢。

针对传统开源网关（如 `new-api` / `One-API`）在超高并发下因**数据库行级锁争抢导致的性能雪崩**，以及新兴微代理（如 `Magpie`）因**重型外部 Node/Bun 进程 IPC 带来的通信抖动与臃肿依赖**，Universal AI Gateway 确立了**纯 Go 单静态二进制、控制面与数据面物理彻底解耦、信封加密、微秒级 WASM 插件沙箱**的核心设计原则。

```
+----------------------------------------------------------------------------------------------------+
|                                    核心指标与工业级特性矩阵                                         |
+----------------------------------------------------------------------------------------------------+
| [编译构建] 100% 纯 Go 实现 (CGO_ENABLED=0)，无需 GCC/MinGW，单二进制静态跨平台分发 (< 25MB)        |
| [吞吐性能] 数据面内存预留-结算计费，热路径零 DB 行锁；吞吐目标见 06 ADR-10（以基准测试为准）       |
| [持久存储] 纯 Go SQLite (modernc.org/sqlite) WAL；追加式计费流水账 + 可丢弃的异步审计日志流水线     |
| [安全体系] AES-256-GCM 凭据加密（主密钥可接 KMS / OS 密钥库）+ AAD 防密文置换，Token HMAC 索引      |
| [极速扩展] 集成 tetratelabs/wazero 纯 Go WASM 运行时，单进程沙箱（超时、内存上限、实例隔离）        |
| [全维接入] OpenAI / Claude Messages / Gemini / DeepSeek / Ollama；同协议透传，跨协议经 Canonical IR |
+----------------------------------------------------------------------------------------------------+
```

> **[v1.1 设计评审]** 本文档集已完成一轮架构评审，发现的问题与修订后的架构决策（部署档位、计费、透传、调度顺序、密钥管理、合规边界等）
> 汇总于 [`06_architecture_review_and_revisions.md`](./06_architecture_review_and_revisions.md)。**原文与 06 冲突时以 06 为准。**
> v1 定位为 **Standalone 单实例**网关（个人 / 团队 / 本地 Agent）；多副本企业部署属于 v2 的 Cluster 档。

---

## 2. 系统整体架构全景图

系统在物理与逻辑维度严格划分为 **Ingress 入口层**、**数据面极速处理层 (Data Plane)**、**WASM 插件沙箱层**、**调度与路由层 (Scheduler)**、**控制面持久存储层 (Control Plane)** 与 **Egress 出口层**：

```
+----------------------------------------------------------------------------------------------------+
|                              Universal AI Gateway System Architecture                              |
+----------------------------------------------------------------------------------------------------+
                                                  |
           [ 1. Ingress Tier ]                    | Downstream API Clients (OpenAI SDK, Claude Code,
                                                  | Cursor, Cline, Web Apps, Local Agents)
                                                  v
+----------------------------------------------------------------------------------------------------+
|  HTTP / SSE / WebSocket Ingress Handler (net/http + Zero-Allocation Header & Body Buffering)      |
|  - Token Extraction (`Authorization: Bearer` / `x-api-key` / `x-goog-api-key`)                    |
|  - Request Normalization & Context Tracing (TraceID Injection)                                    |
|  - Rate Limiting Verification (Client RPM / TPM Token Bucket)                                      |
+----------------------------------------------------------------------------------------------------+
           |
           v
+----------------------------------------------------------------------------------------------------+
|  [ 2. Data Plane: High-Performance In-Memory Core ]                                                |
|                                                                                                    |
|  +--------------------------------+  +--------------------------------+  +----------------------+  |
|  | Token Hash Fast Index          |  | In-Memory Atomic Quota Engine  |  | Binding Cache Ring   |  |
|  | (Salted SHA-256 Lookup Cache)  |  | (Atomic Balance & Delta Accum) |  | (16,384 Cap LRU)     |  |
|  +--------------------------------+  +--------------------------------+  +----------------------+  |
|                                                                                                    |
|  +----------------------------------------------------------------------------------------------+  |
|  | 3. WASM Plugin Sandbox Tier (tetratelabs/wazero, Zero-CGO, Native JIT Engine)               |  |
|  |   * Hook: OnRequest     -> Prompt Compliance, Header Modification, Dynamic Routing Override  |  |
|  |   * Hook: OnStreamChunk -> Real-time Sensitive Token Streaming Interruption (< 10us latency) |  |
|  |   * Hook: OnResponse    -> Post-audit, Sensitive Word Masking, Watermark Injection          |  |
|  +----------------------------------------------------------------------------------------------+  |
|                                                                                                    |
|  +----------------------------------------------------------------------------------------------+  |
|  | 4. Intelligent Routing & Key Pool Tier (Scheduler)                                           |  |
|  |   * Key Pool Manager: Dynamic Weight Round-Robin, In-flight Counter, Cursor Scan             |  |
|  |   * Circuit Breaker: Exponential Cooldown, Sliding Window Error Detection, Auto-recovery     |  |
|  |   * Intermediate Representation (IR): OpenAI <-> Claude <-> Gemini Multi-way Zero-copy Trans |  |
|  +----------------------------------------------------------------------------------------------+  |
+----------------------------------------------------------------------------------------------------+
           |                                                                  |
           | Async Logging Channel (Cap: 8,192, Non-blocking)                 | Egress Stream / HTTP
           v                                                                  v
+---------------------------------------------------+        +---------------------------------------+
| Async Batch Logging Pipeline                      |        | Upstream AI Providers                 |
| - Channel: make(chan RequestLog, 8192)            |        | - Anthropic Claude API (Direct / Bedrock)|
| - Batch Flusher: 250ms Ticker / 256 Max Threshold |        | - OpenAI API (GPT-4o, o1, o3-mini)    |
| - Overload Backpressure: Atomic Dropped Log Metric|        | - Google Gemini 2.0 API               |
| - Single Multi-Row Precompiled INSERT Statement   |        | - DeepSeek / Moonshot / Local Ollama  |
+---------------------------------------------------+        | - MCP Servers (stdio / SSE / Streamable)|
           |                                                 +---------------------------------------+
           v (Batch Multi-row INSERT)
+----------------------------------------------------------------------------------------------------+
|  [ 5. Control Plane: Pure Go Storage & Security Layer ]                                            |
|                                                                                                    |
|  +----------------------------------------------------------------------------------------------+  |
|  | SQLite Storage Engine (modernc.org/sqlite, Pure Go, Single File `gateway.db`)                |  |
|  | PRAGMA journal_mode = WAL;          PRAGMA busy_timeout = 5000;                              |  |
|  | PRAGMA synchronous = NORMAL;        PRAGMA cache_size = -64000; (64MB Cache)                 |  |
|  | PRAGMA temp_store = MEMORY;         PRAGMA mmap_size = 268435456; (256MB mmap)              |  |
|  +----------------------------------------------------------------------------------------------+  |
|                                                                                                    |
|  +------------------------------+  +-------------------------------+  +-------------------------+  |
|  | Envelope Encryption Core     |  | Periodic Quota Reconciliation |  | Admin Web UI Console   |  |
|  | AES-256-GCM + AAD Binding    |  | (1s Delta Batch Sync Engine)  |  | (Vue3 Embed in Binary)  |  |
|  | 32-Byte Master Key Isolation |  | No DB Row-Lock on Data Plane  |  | Strict Cookie & CSRF    |  |
|  +------------------------------+  +-------------------------------+  +-------------------------+  |
+----------------------------------------------------------------------------------------------------+
```

---

## 3. 文档集技术索引与导读导航

本项目设计方案由 6 份核心技术白皮书与 1 份架构评审修订构成，涵盖了从系统哲学、竞品解构、协议转译、路由调度到存储安全与实施验收的全生命周期：

| 序号 | 核心主题 | 文档文件名 | 核心关注点与技术精髓 |
| :---: | :--- | :--- | :--- |
| **00** | **开发者入职指南、背景渊源与系统设计哲学** | [`00_background_philosophy_and_developer_guide.md`](./00_background_philosophy_and_developer_guide.md) | **全景导读与核心基石**：大模型协议碎片化泥潭复盘；Coding Agent 颠覆性流量特征；七大系统设计哲学；端到端请求旅程心智模型；面向实施工程师的十大代码红线禁忌。 |
| **01** | **竞品架构深度解构与演进复盘** | [`01_competitive_teardowns.md`](./01_competitive_teardowns.md) | **技术选型与历史教训**：深度逆向解构 `new-api`、`sub2api` 与 `yetone/magpie`；剖析 Adaptor 模式、三层模型名解耦矩阵、两阶段配额、Bun 外部进程 IPC 痛点与高并发行锁雪崩根因。 |
| **02** | **协议转译与规范化中间表示引擎** | [`02_protocol_translation_and_ir.md`](./02_protocol_translation_and_ir.md) | **统一协议抽象**：深入定义协议中立的 Canonical IR (`UnifiedMessage` / `UnifiedTool`)；实现 OpenAI (v1/chat, responses)、Claude Messages、Gemini 与 DeepSeek 之间的多路全双工零拷贝转译与 SSE 流式状态机。 |
| **03** | **资源池化、智能调度与弹性容灾** | [`03_scheduling_pooling_and_resilience.md`](./03_scheduling_pooling_and_resilience.md) | **高可用算力治理**：异构账号池化（Direct Key / OAuth 订阅 / 实例部署）；提示词缓存（Prompt Cache）感知路由；三维自适应健康探测与熔断；Agent 交互模式下的会话粘滞与分级降级树。 |
| **04** | **高性能存储、信封加密与 WASM 沙箱** | [`04_storage_security_and_implementation.md`](./04_storage_security_and_implementation.md) | **底座基石与安全蓝图**：纯 Go SQLite WAL 极致参数配置；控制面与数据面彻底分离（彻底消灭热点行锁争抢）；8,192 环形异步非阻塞批量日志流水线；AES-256-GCM 信封加密与 AAD 防篡改；`tetratelabs/wazero` 纯 Go WASM 微秒级安全沙箱；完整 Go DDL 与跨平台单二进制构建全景。 |
| **05** | **从零到一工程落地实施手册与验收标准** | [`05_engineering_implementation_checklist.md`](./05_engineering_implementation_checklist.md) | **手把手实施准绳**：6 个 Sprint 迭代研发矩阵；模块级代码骨架与接口契约；白盒单元测试与混沌故障注入清单（WSAENOBUFS 10055、OAuth RTR 死锁、流式截断）；可量化交付验收标准。 |
| **06** | **架构评审报告与设计修订（v1.1）** | [`06_architecture_review_and_revisions.md`](./06_architecture_review_and_revisions.md) | **评审结论与 ADR**：P0/P1/P2 问题清单；部署档位、预留-结算计费与流水账、同协议透传、调度决策顺序、错误分类、密钥管理、管理面隔离、WASM 资源边界、指标口径与合规边界 11 项架构决策。**与 00–05 冲突时以本文为准。** |

---

## 4. 核心技术对比与方案优势

对比业界主流开源网关与代理方案，Universal AI Gateway 具备压倒性的架构优势：

| 评估维度 | 传统开源网关 (`new-api` / `One-API`) | 外部进程代理 (`Magpie`) | **Universal AI Gateway (本项目)** |
| :--- | :--- | :--- | :--- |
| **构建与分发** | 需配置 MySQL/Redis 依赖或 CGO SQLite，跨平台交叉编译极繁琐 | 需安装 Bun/Node 外部运行时，总分发体积 > 250MB | **纯 Go 零 CGO 单一静态二进制（< 25MB），跨平台 5 秒极速交付** |
| **并发计费架构** | **同步写 DB 行锁（`UPDATE quota`）**，并发 200+ 即引发锁死雪崩 | 无企业级计费体系 | **准入时内存预留、流结束按真实 usage 结算，计费事件批量写入追加式流水账，热路径无 DB 行锁** |
| **请求日志吞吐** | 同步逐行或简易异步写库，峰值极易阻塞网络主循环 | 同步写本地文件，无结构化索引 | **审计日志：8,192 有界队列 + 250ms/256 行批量 INSERT，背压时丢弃并计数；计费事件不丢弃** |
| **密钥存储安全** | 数据库明文存储或简单对称加密，无 AAD 绑定，易被替换盗用 | 配置文件纯明文存储 | **32 字节 Master Key 加密（可接 KMS / OS 密钥库） + AES-256-GCM + AAD 防密文置换** |
| **客户端 Token 安全**| 数据库存储明文 Token，全库泄露即导致全部客户凭证沦陷 | 无客户端 Token 治理体系 | **HMAC-SHA256（pepper 不入库）索引存储，仅保留「前缀 + 6 位随机」展示串，全库泄露亦无法还原明文** |
| **扩展机制与隔离** | 无沙箱，任何业务定制必须强行修改并重新编译 Go 源码 | Bun 外部子进程 + NDJSON IPC，每次调用带来 **2~5ms 延迟抖动** | **内置 `wazero` 纯 Go WASM 沙箱，单进程内存隔离，调用超时/内存上限可控；插件面向 IR 而非原始协议** |
| **流式审查延迟** | 需缓冲多个 chunk 或由 Go 阻塞扫描 | 跨进程管道序列化传输，首字延迟（TTFT）劣化显著 | **OnStreamChunk 进程内审查 + 可配置延迟窗口（处理跨 chunk 敏感词），违规即切断并注入合规终止包** |

---

## 5. 快速上手指南（Quick Start）

### 5.1 单静态二进制编译（无需外部依赖）

在具备 Go 1.24+ 的环境下，直接执行纯静态编译：

```bash
# 克隆仓库并进入根目录
git clone https://github.com/ZhangCurosr/universal-ai-gateway.git
cd universal-ai-gateway

# 编译纯 Go 单静态二进制 (零 CGO，支持任意平台交叉编译)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/gateway ./cmd/gateway
```

### 5.2 启动网关服务

首次运行会自动在数据目录创建 `gateway.db` 数据库并初始化 WAL 模式；主密钥默认写入**数据目录之外**的独立路径（生产环境建议改用 KMS 或操作系统密钥库，见 06 ADR-07）：

```bash
./bin/gateway --data-dir=./data --master-key-file=./secrets/master.key --port=8080
```

服务启动日志输出：
```text
[INFO] Storage initialized: SQLite WAL enabled, cache_size=-64000, mmap=256MB (data-dir locked)
[INFO] Credential encryption ready: master key loaded from ./secrets/master.key (kek_version=1)
[INFO] Wazero WebAssembly runtime initialized (Zero CGO)
[INFO] Quota ledger ready (reserve/settle in memory, ledger flush interval: 250ms)
[INFO] Audit logging pipeline active (channel cap: 8192, batch: 256, interval: 250ms)
[INFO] Data plane listening on http://0.0.0.0:8080
[INFO] Admin plane listening on http://127.0.0.1:8081/admin
[INFO] Bootstrap admin token (shown once): uai_admin_...
```

### 5.3 录入上游模型 Key 并签发客户端凭据

通过内置的管理 API 或嵌入式控制台快速录入上游凭证并生成下游调用 Token：

```bash
# 1. 录入 Anthropic Claude 官方 Key (自动进行 AES-256-GCM 信封加密)
curl -X POST http://127.0.0.1:8081/admin/api/keys \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: <BootstrapAdminToken>" \
  -d '{
    "name": "Claude-Production-Key-01",
    "provider": "anthropic",
    "secret": "sk-ant-api03-xxxxxxxxxxxxxxxxxxxx",
    "weight": 10,
    "max_in_flight": 100
  }'

# 2. 签发下游客户端凭证 (获得 Token: uai_live_8f3a9e1...)
curl -X POST http://127.0.0.1:8081/admin/api/clients \
  -H "Content-Type: application/json" \
  -H "X-Admin-Key: <BootstrapAdminToken>" \
  -d '{
    "name": "Cursor-Developer-Team",
    "rate_limit_rpm": 120,
    "total_quota": 1000000000
  }'
```

### 5.4 下游客户端统一无缝接入示例

> 给 AI 编程助手的一页接入说明：[docs/AI_QUICKSTART.md](docs/AI_QUICKSTART.md)。在其他项目里运行 `gateway connect <claude|codex|opencode|pi> --write`，一次配好该 CLI 的模型后端、MCP 与 `use-gateway` 技能。

#### 示例 A：配置 Claude Code CLI
只需将基础 URL 与 API 密钥指向本地或远程网关。注意 Claude Code 会自行拼接 `/v1/messages`，因此 `ANTHROPIC_BASE_URL` **不要带 `/v1`**；
`ANTHROPIC_AUTH_TOKEN` 以 `Authorization: Bearer` 发送，若改用 `ANTHROPIC_API_KEY` 则以 `x-api-key` 头发送，网关两者都接受：
```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_AUTH_TOKEN="uai_live_8f3a9e1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9"

claude "编写一个基于 Go 语言的高并发滑动窗口限流器"
```

#### 示例 B：标准 OpenAI Python SDK 调用
无论底层上游是 Claude 3.5 Sonnet、Gemini 2.0 还是 DeepSeek-R1，均可通过标准的 OpenAI SDK 访问：
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="uai_live_8f3a9e1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9"
)

# 网关自动完成从 OpenAI 协议向 Claude 原生协议的无损流式转译
response = client.chat.completions.create(
    model="claude-3-5-sonnet-20241022",
    messages=[
        {"role": "system", "content": "你是由 Universal AI Gateway 统一驱动的架构专家。"},
        {"role": "user", "content": "请分析 SQLite WAL 模式在高并发读写下的底层原理。"}
    ],
    stream=True
)

for chunk in response:
    content = chunk.choices[0].delta.content
    if content:
        print(content, end="", flush=True)
```

---

## 6. 开源许可与致谢

本项目采用 Apache 2.0 工业级开源协议。架构设计吸收并升华了众多优秀开源项目的实战经验：
- 借鉴并优化了 `04_TavilyProxyManager` 与 `05_MinerUProxyManager` 在生产环境下验证的 **8,192 环形异步流水线** 与 **SQLite WAL 性能参数**；
- 感谢 [Tetrate](https://github.com/tetratelabs/wazero) 打造的无 CGO 纯 Go WebAssembly 顶级运行时；
- 感谢 [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) 团队对纯 Go 生态编译器工具链的杰出贡献。
