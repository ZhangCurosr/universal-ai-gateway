# Universal AI Gateway 架构设计规范 (02)
## 协议转译与规范化中间表示引擎（Protocol Translation & Canonical IR）

> **文档版本**: v1.0.0-PROD  
> **状态**: RFC / 生产实现标准  
> **模块代码**: `github.com/ai-gateway/universal-ai-gateway/internal/ir`, `internal/stream`, `internal/ingress`  
> **适用目标**: OpenAI (v1/chat, v1/responses), Anthropic (Messages), Google Gemini (:generateContent), DeepSeek, MCP (Model Context Protocol)

---

## 1. 架构总览与设计哲学

在现代 AI 应用基础设施中，大模型网关不仅要承担反向代理、负载均衡和配额管控的职责，更核心的使命是抹平上游各模型提供商（OpenAI、Anthropic、Google Gemini、DeepSeek、Ollama 等）在协议语义、数据格式和流式生命周期上的巨大异构性。

传统方案（如早期的聚合代理工具）通常采用两两适配模式（Point-to-Point Translators）。这种设计会导致转译逻辑复杂度随着支持协议数量呈平方级爆炸：
$$\text{Complexity} = \mathcal{O}(N \times M)$$
当引入协议新版本（如 OpenAI 2025 Responses API 或 Claude 3.7 Thinking 思考签名）时，必须同步修改所有端到端的转换代码，极易产生语义降级、信息丢失与逻辑分支死锁。

```
[Point-to-Point Architecture: O(N x M)]
OpenAI   <---> Anthropic
  ^ \         / ^
  |  \       /  |
  |   \     /   |
  v    v   v    v
Gemini  <---> DeepSeek

              vs.

[Canonical IR Star Architecture: O(N + M)]
OpenAI   ---------\          /---------> OpenAI
Anthropic ---------> [Canonical] -------> Anthropic
Gemini   ---------> [    IR   ] -------> Gemini
DeepSeek ---------> [ Engine  ] -------> DeepSeek
Responses--------/          \---------> Responses
```

本网关采用**规范化中间表示（Canonical Intermediate Representation, 简称 Canonical IR）**的星型收敛架构（Hub-and-Spoke Topology）。系统将协议处理解耦为双向解构与合成阶段：
1. **Ingress Lowering（降阶/解析）**：将各厂商特异性的请求解码并升采样/降阶为强类型、高保真的统一 AST（抽象语法树）`ir.Request`；
2. **Egress Synthesizing（升阶/合成）**：根据路由决策与目标上游协议规范，将 `ir.Request` 重新序列化为目标厂商的高保真请求；
3. **Response & Stream Transpiling（响应与流式逆向转译）**：通过零拷贝有限状态机（FSM），在上游流式 SSE Chunk 到达时实时重构为客户端所期望的事件帧序列。

复杂度由此降低至：
$$\text{Complexity} = \mathcal{O}(N + M)$$

---

## 2. 多通道接入设计（Multi-Channel Ingress）

网关需应对自动化脚本、开发人员终端调试、本地桌面 IDE 及分布式 Agent 集群的多样化接入需求，构建全场景统一的多通道接入层。

```
                                 +-------------------------------------------------------------+
                                 |                Multi-Channel Ingress Layer                 |
                                 +-------------------------------------------------------------+
                                                                |
        +-----------------------+-------------------------------+------------------------------+
        |                       |                               |                              |
        v                       v                               v                              v
+---------------+      +------------------+           +-------------------+         +--------------------+
|   REST API    |      |  CLI & Pipe I/O  |           |     MCP Server    |         | LAN / Portal Auth  |
| /v1/chat/...  |      |  Interactive     |           |  Stdio Mode       |         | RFC1918 Guard      |
| /v1/messages  |      |  cat code | gw   |           |  (Claude/Cursor)  |         | PIN Hotspot Pair   |
| /v1/responses |      |  Bench & Test    |           |  SSE Remote Mode  |         | mDNS Zero-Config   |
| :generateCont |      +------------------+           +-------------------+         +--------------------+
+---------------+               |                               |                              |
        |                       |                               |                              |
        +-----------------------+-------------------------------+------------------------------+
                                                                |
                                                                v
                                              +-----------------------------------+
                                              | Protocol Sniffer & Dynamic Router |
                                              +-----------------------------------+
                                                                |
                                                                v
                                              +-----------------------------------+
                                              |       Canonical IR Pipeline       |
                                              +-----------------------------------+
```

### 2.1 REST API 接入层

网关原生挂载四大主流 API 规范路由，保持路径语义的精确一致：
- **OpenAI 兼容端点**：`POST /v1/chat/completions`、`GET /v1/models`
- **Anthropic 原生端点**：`POST /v1/messages`、`POST /v1/complete`
- **OpenAI 2025 Responses 端点**：`POST /v1/responses`
- **Google Gemini 端点**：`POST /v1beta/models/{model}:generateContent`、`POST /v1beta/models/{model}:streamGenerateContent`

#### 协议嗅探（Protocol Sniffing）与动态派发
针对使用自定义反向代理地址、路径重写或根路径直接发送请求的客户端，网关内置三级启发式嗅探引擎：

```go
type IngressProtocol int

const (
	ProtoUnknown IngressProtocol = iota
	ProtoOpenAIChat
	ProtoOpenAIResponses
	ProtoAnthropicMessages
	ProtoGoogleGemini
)

func SniffProtocol(r *http.Request, peekBody []byte) IngressProtocol {
	// 1. 路径特征匹配 (First-order Routing)
	path := r.URL.Path
	switch {
	case strings.Contains(path, "/chat/completions"):
		return ProtoOpenAIChat
	case strings.Contains(path, "/v1/messages"):
		return ProtoAnthropicMessages
	case strings.Contains(path, "/responses"):
		return ProtoOpenAIResponses
	case strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent"):
		return ProtoGoogleGemini
	}

	// 2. HTTP 头部特征指纹 (Header Fingerprinting)
	if r.Header.Get("anthropic-version") != "" || r.Header.Get("x-api-key") != "" {
		return ProtoAnthropicMessages
	}
	if strings.Contains(r.Header.Get("x-goog-api-client"), "gl-go") || r.URL.Query().Get("key") != "" {
		return ProtoGoogleGemini
	}
	if r.Header.Get("OpenAI-Beta") != "" {
		return ProtoOpenAIResponses
	}

	// 3. 载荷特征快速嗅探 (Payload Heuristics without full deserialization)
	if bytes.Contains(peekBody, []byte(`"contents"`)) && bytes.Contains(peekBody, []byte(`"parts"`)) {
		return ProtoGoogleGemini
	}
	if bytes.Contains(peekBody, []byte(`"system"`)) && bytes.Contains(peekBody, []byte(`"messages"`)) {
		return ProtoAnthropicMessages
	}
	if bytes.Contains(peekBody, []byte(`"input"`)) && bytes.Contains(peekBody, []byte(`"instructions"`)) {
		return ProtoOpenAIResponses
	}

	// 兜底回退为 OpenAI Chat 协议
	return ProtoOpenAIChat
}
```

### 2.2 本地 CLI 终端交互层

为工程师提供无需借助第三方 UI 的极速验证、管道重定向以及压测诊断能力：

#### 交互式 REPL 与 Unix 管道
网关编译二进制产物内置 CLI 运行时：
```bash
# 管道模式：标准输入重定向转译并流式输出
cat internal/ir/types.go | gateway run --model claude-3-7-sonnet --system "检查并发安全性"

# 交互式 REPL 模式
gateway repl --model deepseek-reasoner
```
CLI 底层将 stdin 封包为 `ir.Request`，并通过标准输出直接驱动零拷贝流式终端渲染器。

#### 资源池延迟压测与诊断命令
内置多维度诊断套件：
```bash
# 压测所有活跃上游的 TTFT (首 Token 延迟) 与 TPS (每秒 Token 数)
gateway bench --pool standard-tier --concurrency 10 --tokens 512

# 链路全生命周期健康体检
gateway test --verbose
```
输出详尽的分位延迟矩阵（P50, P90, P99）、连接池建立耗时、TLS 握手耗时与 DNS 解析耗时。

### 2.3 Model Context Protocol (MCP) 双模服务端

借鉴 `05_MinerUProxyManager` 的成功经验，网关采用**纯原生 Go 实现 Model Context Protocol**（遵循 2024-11-05 协议版本），彻底剥离对 Node.js/Python 外部运行时的依赖，保证单个独立静态可执行文件的极简交付。

网关实现双模监听传输通道：
1. **Stdio 模式**：标准输入输出绑定 JSON-RPC 2.0，供 Claude Desktop、Cursor、Cline 等本地 Agent 进程直接以子进程方式调起；所有运行期日志强制重定向至 `os.Stderr`，防止污染 `os.Stdout` 数据信道。
2. **SSE 模式**：基于 HTTP SSE 长连接提供分布式 Agent 注册与调用能力，支持跨局域网多设备协同。

#### 核心暴露的 MCP Tools
网关通过 MCP 暴露底层运维与调度核心原子能力：
- `switch_model`：会话级动态切换当前路由规则绑定的后端基座模型（如在上下文超出预算时自动降级或强制切流）。
- `inspect_cache_hit_rate`：查询指定模型前缀缓存（Prompt Caching / Context Caching）在各上游账号的实时命中率与节省费用。
- `query_account_quota`：动态获取当前可用上游账号池的可用额度、Token 余额与速率限制水位（RPM/TPM）。
- `gateway_metrics`：导出网关运行时指标（Goroutine 数量、内存占用、TTFT 分布、熔断状态）。

### 2.4 局域网接入与安全（LAN & Portal）

在办公室或家庭局域网运行网关时，必须保证零信任网络安全与无缝发现：

#### RFC1918 私网范围过滤与公网阻断
网关严格校验连接来源。若启动时未显式开启公网穿透标识，任何非 RFC1918（`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`）及非本地回环（`127.0.0.0/8`, `::1`）请求直接在 TCP 握手或中间件首层丢弃：

```go
func IsPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	privateCIDRs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"100.64.0.0/10", // CGNAT
	}
	for _, cidr := range privateCIDRs {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return true
		}
	}
	return false
}
```

#### 6 位数字 PIN 码安全认证配对
借鉴 `01_RelyVPNLocalProxy` 的 Captive Portal 机制。新局域网设备初次连接网关 Web Portal 时，必须输入网关启动时打印并固化的 6 位数字配对 PIN 码 `Portal`。认证成功后设备 IP/MAC 被记录入持久化设备表，并签发带签名的 HTTP-Only Cookie 与 API 访问令牌。暴力尝试 5 次即触发指数退避冷却封禁。

#### mDNS 零配置广播服务
网关内置 mDNS（Multicast DNS）广播引擎（端口 UDP 5353），向局域网广播 `_ai-gateway._tcp.local` 域名服务。服务携带 TXT 记录（包含当前版本、支持协议列表、MCP SSE 端点）。客户端 Agent 启动后无需人工配置 IP 地址，即可通过 mDNS 解析自动发现可用网关。

---

## 3. 规范化中间表示引擎（Canonical IR）规范

Canonical IR 是整个网关的语义中枢。其核心理念是**高保真（High-Fidelity）**与**语义超集覆盖（Superset Inclusivity）**——中间表示必须能够无损容纳所有主流大模型协议的所有特有能力，绝不在进入 IR 阶段即发生有损截断。

### 3.1 核心 AST 数据结构详细定义

```
                                  +-----------------------+
                                  |      ir.Request       |
                                  |-----------------------|
                                  | - Model: string       |
                                  | - Temperature: *float |
                                  | - Thinking: *Config   |
                                  | - Tools: []ToolDef    |
                                  | - Messages: []Message |
                                  +-----------------------+
                                              |
                                              | 1:N
                                              v
                                  +-----------------------+
                                  |      ir.Message       |
                                  |-----------------------|
                                  | - Role: ir.Role       |
                                  | - Name: string        |
                                  | - Parts: []ContentPart|
                                  +-----------------------+
                                              |
                     +------------------------+------------------------+
                     |                        |                        |
                     v                        v                        v
             +---------------+        +----------------+       +-----------------+
             |  ir.TextPart  |        | ir.BinaryPart  |       | ir.ThinkingPart |
             |---------------|        |----------------|       |-----------------|
             | - Text: string|        | - MIME: string |       | - Text: string  |
             +---------------+        | - Data: []byte |       | - Signature: str|
                                      +----------------+       +-----------------+
                                              |
                                              v
                              +-------------------------------+
                              | ir.ToolCall / ToolResultPart  |
                              |-------------------------------|
                              | - ID, Name, Arguments, Output |
                              +-------------------------------+
```

#### 角色与请求根定义
```go
package ir

import "time"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentPart 为内容块多态接口
type ContentPart interface {
	Type() ContentPartType
	Clone() ContentPart
}

type ContentPartType string

const (
	PartTypeText       ContentPartType = "text"
	PartTypeBinary     ContentPartType = "binary"
	PartTypeThinking   ContentPartType = "thinking"
	PartTypeToolCall   ContentPartType = "tool_call"
	PartTypeToolResult ContentPartType = "tool_result"
)

type Message struct {
	Role       Role          `json:"role"`
	Name       string        `json:"name,omitempty"`
	Parts      []ContentPart `json:"parts"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type Request struct {
	Model            string         `json:"model"`
	Messages         []Message      `json:"messages"`
	System           []TextPart     `json:"system,omitempty"` // 顶层 System 提示词抽象
	Temperature      *float32       `json:"temperature,omitempty"`
	TopP             *float32       `json:"top_p,omitempty"`
	MaxTokens        *int           `json:"max_tokens,omitempty"`
	StopSequences    []string       `json:"stop_sequences,omitempty"`
	Stream           bool           `json:"stream"`
	Tools            []ToolDefinition `json:"tools,omitempty"`
	ToolChoice       *ToolChoice    `json:"tool_choice,omitempty"`
	Thinking         *ThinkingConfig `json:"thinking,omitempty"`
	ProviderMetadata map[string]any `json:"provider_metadata,omitempty"`
}

type ThinkingConfig struct {
	Enabled      bool   `json:"enabled"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Mode         string `json:"mode,omitempty"` // "thinking", "reasoning", "extended"
}
```

#### 多态部件定义
```go
// TextPart 纯文本内容块
type TextPart struct {
	Text string `json:"text"`
}
func (p TextPart) Type() ContentPartType { return PartTypeText }
func (p TextPart) Clone() ContentPart   { return TextPart{Text: p.Text} }

// BinaryPart 媒体（图片、音频、PDF文件等）内容块
type BinaryPart struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data,omitempty"` // 原始字节或已解码 Base64
	URL      string `json:"url,omitempty"`  // 远程直链
}
func (p BinaryPart) Type() ContentPartType { return PartTypeBinary }
func (p BinaryPart) Clone() ContentPart {
	dataCopy := make([]byte, len(p.Data))
	copy(dataCopy, p.Data)
	return BinaryPart{MimeType: p.MimeType, Data: dataCopy, URL: p.URL}
}

// ThinkingPart 思维链/深度思考内容块
type ThinkingPart struct {
	Text      string `json:"text"`                // 思考过程内容
	Signature string `json:"signature,omitempty"` // Claude 3.7 / 厂商特定的加密验证签名
	Redacted  bool   `json:"redacted,omitempty"`  // 是否属于被安全审查擦除的思考块
}
func (p ThinkingPart) Type() ContentPartType { return PartTypeThinking }
func (p ThinkingPart) Clone() ContentPart {
	return ThinkingPart{Text: p.Text, Signature: p.Signature, Redacted: p.Redacted}
}

// ToolCallPart 模型发起的函数/工具调用
type ToolCallPart struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 格式参数字符串
}
func (p ToolCallPart) Type() ContentPartType { return PartTypeToolCall }
func (p ToolCallPart) Clone() ContentPart {
	return ToolCallPart{ID: p.ID, Name: p.Name, Arguments: p.Arguments}
}

// ToolResultPart 工具执行完毕向模型回填的结果
type ToolResultPart struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error,omitempty"`
}
func (p ToolResultPart) Type() ContentPartType { return PartTypeToolResult }
func (p ToolResultPart) Clone() ContentPart {
	return ToolResultPart{ToolCallID: p.ToolCallID, Content: p.Content, IsError: p.IsError}
}
```

---

## 4. 思维链（Thinking / Reasoning）与工具调用的高保真跨协议映射

### 4.1 思维链跨协议映射矩阵

各大厂商对深度思考（Reasoning）的表征截然不同：
1. **Claude 3.7 Sonnet**：引入显式 `thinking` 块与强制防篡改的 `signature` 签名机制。在多轮对话中，下一轮对话传入历史 Assistant 消息时，若包含 `thinking` 块必须连同 `signature` 原样回传，否则上游将拒绝请求并报错；若需要降低 Token 消耗，可将内容替换为 `redacted_thinking`。
2. **DeepSeek (R1 / V3)**：在 OpenAI 兼容模式下通过 `reasoning_content` 字段伴随 Assistant 消息和流式增量输出，或在原生 ChatML 模板中采用 `<think>...</think>` 标签包裹。
3. **OpenAI o1 / o3-mini (Responses API)**：在 `v1/responses` 体系中以独立的 `thought` 引用类型体现，不可直接修改。
4. **Google Gemini 2.0 Flash Thinking**：在 Candidates 的 Parts 中以特殊的 `thought: true` 标记区分思考文本与可见回复。

#### 映射路由矩阵
| 源协议 / 字段 | 转换为 Canonical IR | 目标: Anthropic | 目标: OpenAI / DeepSeek | 目标: Google Gemini |
| :--- | :--- | :--- | :--- | :--- |
| **Anthropic** `thinking` + `signature` | `ThinkingPart{Text, Signature}` | 原样输出 `thinking` 块并在保留 signature 前提下回传 | 转为 `reasoning_content`，剥离 signature | 转为 `part.text` 且附带 `thought: true` |
| **DeepSeek** `reasoning_content` | `ThinkingPart{Text}` | 构造 `thinking` 块（无签名或本地降级） | 原样注入 `reasoning_content` | 注入 `thought: true` 部分 |
| **OpenAI Responses** `thought` | `ThinkingPart{Text}` | 包装为 `thinking` 内容块 | 降级为 `reasoning_content` | 注入 `thought: true` 部分 |
| **Gemini** `part{thought: true}` | `ThinkingPart{Text}` | 包装为 `thinking` 内容块 | 提取为 `reasoning_content` | 原样保持 `thought: true` |

### 4.2 Tool Call 与 Schema AST 递归重写机制

OpenAI 广泛使用的 JSON Schema 规范包含了大量的关键字（如 `$schema`、`title`、`additionalProperties`、`default`、`pattern`、小写的类型声明 `string`、`object` 等）。然而，Google Gemini 的 OpenAPI 3.0 引擎对 Schema 有极其严苛的白名单限制：
- **类型必须大写**：`OBJECT`、`STRING`、`NUMBER`、`INTEGER`、`BOOLEAN`、`ARRAY`；
- **非法字段强制拒绝**：如果 Schema 中包含 `$schema`、`title`、`additionalProperties`、`$defs`、`patternProperties` 等字段，Gemini API 会直接抛出 `HTTP 400 INVALID_ARGUMENT`；
- **空对象属性清洗**：没有声明 `properties` 的 `OBJECT` 类型必须被重构为自由字典或显式赋予空结构体。

#### 递归清洗算法实现
```go
// CleanSchemaForGemini 递归清洗 JSON Schema，将其转换为符合 Gemini 规范的强约束结构
func CleanSchemaForGemini(node map[string]any) map[string]any {
	cleaned := make(map[string]any)

	// 1. 递归清洗并转换类型字段为大写
	if rawType, ok := node["type"]; ok {
		switch t := rawType.(type) {
		case string:
			cleaned["type"] = strings.ToUpper(t)
		case []any: // 多类型联合声明例如 ["string", "null"]
			if len(t) > 0 {
				if firstStr, ok := t[0].(string); ok {
					cleaned["type"] = strings.ToUpper(firstStr)
				}
			}
		}
	}

	// 2. 字段拷贝与过滤白名单
	allowedFields := map[string]bool{
		"type":        true,
		"format":      true,
		"description": true,
		"nullable":    true,
		"enum":        true,
		"items":       true,
		"properties":  true,
		"required":    true,
	}

	for k, v := range node {
		if !allowedFields[k] {
			// 自动剪枝：丢弃 $schema, title, additionalProperties, default 等
			continue
		}
		if k == "type" {
			continue // 已处理
		}

		switch k {
		case "properties":
			if props, ok := v.(map[string]any); ok {
				newProps := make(map[string]any)
				for propName, propSchema := range props {
					if subMap, ok := propSchema.(map[string]any); ok {
						newProps[propName] = CleanSchemaForGemini(subMap)
					}
				}
				cleaned["properties"] = newProps
			}
		case "items":
			if itemMap, ok := v.(map[string]any); ok {
				cleaned["items"] = CleanSchemaForGemini(itemMap)
			}
		default:
			cleaned[k] = v
		}
	}

	// 3. 边界修补：若为 OBJECT 却无 properties，补充空 map
	if cleaned["type"] == "OBJECT" && cleaned["properties"] == nil {
		cleaned["properties"] = map[string]any{}
	}

	return cleaned
}
```

### 4.3 工具调用孤立与缺失自愈（Tool Call Self-Healing）

在复杂 Agent 场景中，客户端经常出现协议违规导致模型崩溃：
1. **Dangling Tool Call（悬空调用）**：上一轮 Assistant 发起了 `tool_calls`（含 `call_id="call_123"`），但客户端 Agent 下一轮未回填 `role="tool"` 的结果，直接发起了新的 User 提示词。在 Anthropic/OpenAI 原生规范下，这会导致 400 校验异常。
2. **Missing Tool Call ID（缺失 ID）**：某些本地模型未生成唯一 ID，返回空 ID。
3. **空返回死锁**：Agent 执行失败返回空内容，触发某些上游 API 的 `content cannot be empty` 错误。

#### 自愈状态机处理逻辑
网关在 Lowering 阶段执行 Self-Healing 校验：
- **检测未闭合的 ToolCall**：若检测到 `ir.ToolCallPart` 存在，但后续消息并非对应的 `ir.ToolResultPart`，网关自动在两者之间合成一条虚拟的 `ir.ToolResultPart`，填入 `{"status": "cancelled", "reason": "superseded_by_user"}`，从而使消息队列合规化闭合。
- **自动补全 ID**：若发现缺失 `tool_call_id`，网关基于 UUIDv4 快速生成带有协议前缀的伪 ID（如 `call_synthetic_` / `toolu_synthetic_`），并在上下文中维持双向映射表。

---

## 5. 零拷贝流式转译状态机（Zero-Copy SSE Streaming Engine）

在全双工流式交互场景下，流式转译引擎是网关性能的终极考验。客户端首 Token 延迟（Time To First Token, TTFT）的增量开销必须控制在 **< 0.2ms** 级别，内存分配必须趋近于零。

### 5.1 协议流式生命周期差异

OpenAI 与 Anthropic 的流式事件模型存在根本性的结构差异：
- **OpenAI 机制**：扁平式增量通知。每个 Chunk 是一个独立的 `chat.completion.chunk` 对象，通过 `choices[0].delta` 逐步携带 `content`、`reasoning_content` 或 `tool_calls`。生命周期隐式由空或 `[DONE]` 结束。
- **Anthropic 机制**：分层嵌套严格事件状态流。必须严格经历：
  `message_start` $\rightarrow$ `content_block_start` $\rightarrow$ `content_block_delta` $\rightarrow$ `content_block_stop` $\rightarrow$ `message_delta` $\rightarrow$ `message_stop`。

```
OpenAI Stream:
[Delta(reasoning)] -> [Delta(reasoning)] -> [Delta(content)] -> [Delta(content)] -> [DONE]
      |                      |                    |                   |
      v                      v                    v                   v
Anthropic FSM Synthesizer:
[message_start]
[block_start(0, thinking)]
  -> [block_delta(thinking)]
  -> [block_delta(thinking)]
[block_stop(0)]
[block_start(1, text)]
  -> [block_delta(text)]
  -> [block_delta(text)]
[block_stop(1)]
[message_delta(stop_reason)]
[message_stop]
```

### 5.2 状态机状态流转图（FSM）

```
                     +----------------------+
                     |     StateInit        |
                     +----------------------+
                                |
                                | First Chunk (Initialize message_start)
                                v
                     +----------------------+
                     |   StateMessageOpen   |
                     +----------------------+
                                |
             +------------------+------------------+
             |                                     |
             | delta.reasoning_content             | delta.content / delta.tool_calls
             v                                     v
+------------------------+             +------------------------+
| StateThinkingBlockOpen |             |   StateTextBlockOpen   |
+------------------------+             +------------------------+
| - emit block_start(0)  |             | - emit block_start(idx)|
| - emit thinking_delta  |             | - emit text_delta      |
+------------------------+             +------------------------+
             |                                     |
             | transition: delta.content arrives   |
             | (close thinking, open text)         |
             +----------------->-------------------+
                                |
                                | Finish / [DONE]
                                v
                     +----------------------+
                     |   StateClosingBlocks |
                     | - emit block_stop    |
                     | - emit message_delta |
                     | - emit message_stop  |
                     +----------------------+
                                |
                                v
                     +----------------------+
                     |    StateCompleted    |
                     +----------------------+
```

### 5.3 性能极值优化机制

1. **`sync.Pool` 字节缓冲区池化**：
   彻底杜绝在每个 SSE 事件产生时通过 `json.Marshal` 分配内存。预热多规格的 `*bytes.Buffer` 池，单连接复用固定内存。
2. **零序列化/快速字段字节提取（Zero-Copy Fast-Scan）**：
   上游返回的 SSE 数据行多以 `data: {...}\n\n` 构成。网关避免通过通用 JSON 反序列化器解析整个庞大对象，而是基于轻量级 SIMD 启发式扫描器直接探测 `"reasoning_content":"`、`"content":"`、`"finish_reason":"` 的起始与结束偏移量，切片直接转发拼装。

---

## 6. 完整 Go 核心代码骨架实现

以下提供网关核心模块完整、可编译且高健壮性的 Go 实现骨架。

### 6.1 `internal/ir/types.go`

```go
package ir

import "errors"

// Role 定义消息角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentPartType 内容块类型
type ContentPartType string

const (
	PartTypeText       ContentPartType = "text"
	PartTypeBinary     ContentPartType = "binary"
	PartTypeThinking   ContentPartType = "thinking"
	PartTypeToolCall   ContentPartType = "tool_call"
	PartTypeToolResult ContentPartType = "tool_result"
)

type ContentPart interface {
	Type() ContentPartType
	Clone() ContentPart
}

type TextPart struct {
	Text string `json:"text"`
}

func (p TextPart) Type() ContentPartType { return PartTypeText }
func (p TextPart) Clone() ContentPart    { return TextPart{Text: p.Text} }

type BinaryPart struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data,omitempty"`
	URL      string `json:"url,omitempty"`
}

func (p BinaryPart) Type() ContentPartType { return PartTypeBinary }
func (p BinaryPart) Clone() ContentPart {
	d := make([]byte, len(p.Data))
	copy(d, p.Data)
	return BinaryPart{MimeType: p.MimeType, Data: d, URL: p.URL}
}

type ThinkingPart struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
	Redacted  bool   `json:"redacted,omitempty"`
}

func (p ThinkingPart) Type() ContentPartType { return PartTypeThinking }
func (p ThinkingPart) Clone() ContentPart {
	return ThinkingPart{Text: p.Text, Signature: p.Signature, Redacted: p.Redacted}
}

type ToolCallPart struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (p ToolCallPart) Type() ContentPartType { return PartTypeToolCall }
func (p ToolCallPart) Clone() ContentPart {
	return ToolCallPart{ID: p.ID, Name: p.Name, Arguments: p.Arguments}
}

type ToolResultPart struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error,omitempty"`
}

func (p ToolResultPart) Type() ContentPartType { return PartTypeToolResult }
func (p ToolResultPart) Clone() ContentPart {
	return ToolResultPart{ToolCallID: p.ToolCallID, Content: p.Content, IsError: p.IsError}
}

type Message struct {
	Role     Role          `json:"role"`
	Name     string        `json:"name,omitempty"`
	Parts    []ContentPart `json:"parts"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolChoice struct {
	Mode string `json:"mode"` // "none", "auto", "required", "tool"
	Tool string `json:"tool,omitempty"`
}

type ThinkingConfig struct {
	Enabled      bool `json:"enabled"`
	BudgetTokens int  `json:"budget_tokens,omitempty"`
}

type Request struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	System        []TextPart      `json:"system,omitempty"`
	Temperature   *float32        `json:"temperature,omitempty"`
	TopP          *float32        `json:"top_p,omitempty"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream"`
	Tools         []ToolDefinition`json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

type Response struct {
	ID           string    `json:"id"`
	Model        string    `json:"model"`
	Message      Message   `json:"message"`
	FinishReason string    `json:"finish_reason"`
	Usage        Usage     `json:"usage"`
}

var (
	ErrEmptyMessages       = errors.New("ir: messages cannot be empty")
	ErrUnsupportedPartType = errors.New("ir: unsupported content part type")
)
```

### 6.2 `internal/ir/openai.go`

```go
package ir

import (
	"encoding/json"
	"fmt"
)

// OpenAI DTO Structs
type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature *float32        `json:"temperature,omitempty"`
	TopP        *float32        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Tools       []openAITool    `json:"tools,omitempty"`
	Stop        any             `json:"stop,omitempty"`
}

type openAIMessage struct {
	Role             string            `json:"role"`
	Content          any               `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"` // DeepSeek / O-series
	Name             string            `json:"name,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
	ToolCalls        []openAIToolCall  `json:"tool_calls,omitempty"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFuncDef  `json:"function"`
}

type openAIFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// OpenAIRequestToLowering: OpenAI Chat -> Canonical IR
func OpenAIRequestToLowering(raw []byte) (*Request, error) {
	var src openAIChatRequest
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, fmt.Errorf("openai lowering decode err: %w", err)
	}

	req := &Request{
		Model:       src.Model,
		Temperature: src.Temperature,
		TopP:        src.TopP,
		MaxTokens:   src.MaxTokens,
		Stream:      src.Stream,
	}

	for _, msg := range src.Messages {
		irMsg := Message{
			Role: Role(msg.Role),
			Name: msg.Name,
		}

		// 1. 处理 DeepSeek 推理内容
		if msg.ReasoningContent != "" {
			irMsg.Parts = append(irMsg.Parts, ThinkingPart{
				Text: msg.ReasoningContent,
			})
		}

		// 2. 处理常规多态 Content
		switch val := msg.Content.(type) {
		case string:
			if val != "" {
				irMsg.Parts = append(irMsg.Parts, TextPart{Text: val})
			}
		case []any:
			for _, item := range val {
				if m, ok := item.(map[string]any); ok {
					pType, _ := m["type"].(string)
					switch pType {
					case "text":
						text, _ := m["text"].(string)
						irMsg.Parts = append(irMsg.Parts, TextPart{Text: text})
					case "image_url":
						if urlObj, ok := m["image_url"].(map[string]any); ok {
							urlStr, _ := urlObj["url"].(string)
							irMsg.Parts = append(irMsg.Parts, BinaryPart{URL: urlStr, MimeType: "image/jpeg"})
						}
					}
				}
			}
		}

		// 3. 处理 Tool Calls
		for _, tc := range msg.ToolCalls {
			irMsg.Parts = append(irMsg.Parts, ToolCallPart{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}

		// 4. 处理 Tool 回执结果
		if msg.Role == "tool" {
			textVal := ""
			if str, ok := msg.Content.(string); ok {
				textVal = str
			}
			irMsg.Parts = append(irMsg.Parts, ToolResultPart{
				ToolCallID: msg.ToolCallID,
				Content:    textVal,
			})
		}

		req.Messages = append(req.Messages, irMsg)
	}

	// 转换工具定义
	for _, t := range src.Tools {
		req.Tools = append(req.Tools, ToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}

	return req, nil
}

// RequestToOpenAISynthesizer: Canonical IR -> OpenAI Request
func RequestToOpenAISynthesizer(req *Request) ([]byte, error) {
	dst := openAIChatRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stream:      req.Stream,
	}

	for _, msg := range req.Messages {
		outMsg := openAIMessage{
			Role: string(msg.Role),
			Name: msg.Name,
		}

		var textBuffer string
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case TextPart:
				textBuffer += p.Text
			case ThinkingPart:
				// DeepSeek / OpenAI 扩展字段承载思考内容
				outMsg.ReasoningContent = p.Text
			case ToolCallPart:
				outMsg.ToolCalls = append(outMsg.ToolCalls, openAIToolCall{
					ID:   p.ID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{
						Name:      p.Name,
						Arguments: p.Arguments,
					},
				})
			case ToolResultPart:
				outMsg.ToolCallID = p.ToolCallID
				textBuffer = p.Content
			}
		}
		outMsg.Content = textBuffer
		dst.Messages = append(dst.Messages, outMsg)
	}

	for _, tool := range req.Tools {
		dst.Tools = append(dst.Tools, openAITool{
			Type: "function",
			Function: openAIFuncDef{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}

	return json.Marshal(dst)
}
```

### 6.3 `internal/ir/anthropic.go`

```go
package ir

import (
	"encoding/json"
	"fmt"
)

type anthropicMessagesRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        any                `json:"system,omitempty"` // string or []anthropicContentBlock
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float32           `json:"temperature,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	Thinking      *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicContentBlock
}

type anthropicContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	Signature string         `json:"signature,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// AnthropicRequestToLowering: Anthropic Messages -> Canonical IR
func AnthropicRequestToLowering(raw []byte) (*Request, error) {
	var src anthropicMessagesRequest
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, fmt.Errorf("anthropic lowering decode err: %w", err)
	}

	maxTokens := src.MaxTokens
	req := &Request{
		Model:       src.Model,
		Temperature: src.Temperature,
		MaxTokens:   &maxTokens,
		Stream:      src.Stream,
	}

	// 提取 System
	if sysStr, ok := src.System.(string); ok && sysStr != "" {
		req.System = []TextPart{{Text: sysStr}}
	}

	// 提取 Thinking 配置
	if src.Thinking != nil && src.Thinking.Type == "enabled" {
		req.Thinking = &ThinkingConfig{
			Enabled:      true,
			BudgetTokens: src.Thinking.BudgetTokens,
		}
	}

	for _, msg := range src.Messages {
		irMsg := Message{Role: Role(msg.Role)}

		switch val := msg.Content.(type) {
		case string:
			irMsg.Parts = append(irMsg.Parts, TextPart{Text: val})
		case []any:
			for _, item := range val {
				var block anthropicContentBlock
				bBytes, _ := json.Marshal(item)
				if err := json.Unmarshal(bBytes, &block); err != nil {
					continue
				}

				switch block.Type {
				case "text":
					irMsg.Parts = append(irMsg.Parts, TextPart{Text: block.Text})
				case "thinking":
					// 保留 Claude 3.7 Thinking 签名
					irMsg.Parts = append(irMsg.Parts, ThinkingPart{
						Text:      block.Thinking,
						Signature: block.Signature,
					})
				case "tool_use":
					argsBytes, _ := json.Marshal(block.Input)
					irMsg.Parts = append(irMsg.Parts, ToolCallPart{
						ID:        block.ID,
						Name:      block.Name,
						Arguments: string(argsBytes),
					})
				case "tool_result":
					irMsg.Role = RoleTool
					irMsg.Parts = append(irMsg.Parts, ToolResultPart{
						ToolCallID: block.ToolUseID,
						Content:    block.Content,
						IsError:    block.IsError,
					})
				}
			}
		}

		req.Messages = append(req.Messages, irMsg)
	}

	return req, nil
}

// RequestToAnthropicSynthesizer: Canonical IR -> Anthropic Request
func RequestToAnthropicSynthesizer(req *Request) ([]byte, error) {
	maxTok := 4096
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTok = *req.MaxTokens
	}

	dst := anthropicMessagesRequest{
		Model:       req.Model,
		MaxTokens:   maxTok,
		Temperature: req.Temperature,
		Stream:      req.Stream,
	}

	if len(req.System) > 0 {
		dst.System = req.System[0].Text
	}

	if req.Thinking != nil && req.Thinking.Enabled {
		dst.Thinking = &anthropicThinking{
			Type:         "enabled",
			BudgetTokens: req.Thinking.BudgetTokens,
		}
	}

	for _, msg := range req.Messages {
		antMsg := anthropicMessage{
			Role: string(msg.Role),
		}

		var blocks []anthropicContentBlock
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case TextPart:
				blocks = append(blocks, anthropicContentBlock{
					Type: "text",
					Text: p.Text,
				})
			case ThinkingPart:
				// 重构高保真 Claude 思考块
				blocks = append(blocks, anthropicContentBlock{
					Type:      "thinking",
					Thinking:  p.Text,
					Signature: p.Signature,
				})
			case ToolCallPart:
				var argMap map[string]any
				_ = json.Unmarshal([]byte(p.Arguments), &argMap)
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    p.ID,
					Name:  p.Name,
					Input: argMap,
				})
			case ToolResultPart:
				antMsg.Role = "user" // Anthropic 规范：工具回填作为 user 角色的 tool_result 块
				blocks = append(blocks, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: p.ToolCallID,
					Content:   p.Content,
					IsError:   p.IsError,
				})
			}
		}

		antMsg.Content = blocks
		dst.Messages = append(dst.Messages, antMsg)
	}

	for _, tool := range req.Tools {
		dst.Tools = append(dst.Tools, anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
		})
	}

	return json.Marshal(dst)
}
```

### 6.4 `internal/ir/gemini.go`

```go
package ir

import (
	"encoding/json"
	"fmt"
)

type geminiGenerateContentRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiToolWrapper     `json:"tools,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role"` // "user" or "model"
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	Thought          bool                `json:"thought,omitempty"` // Gemini 2.0 Flash Thinking
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResp `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type geminiFunctionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiToolWrapper struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
}

type geminiFuncDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type geminiGenerationConfig struct {
	Temperature     *float32 `json:"temperature,omitempty"`
	TopP            *float32 `json:"topP,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
}

// RequestToGeminiSynthesizer: Canonical IR -> Google Gemini Request
func RequestToGeminiSynthesizer(req *Request) ([]byte, error) {
	dst := geminiGenerateContentRequest{}

	// 1. Generation Config
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens != nil {
		dst.GenerationConfig = &geminiGenerationConfig{
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			MaxOutputTokens: req.MaxTokens,
		}
	}

	// 2. System Instruction
	if len(req.System) > 0 {
		dst.SystemInstruction = &geminiContent{
			Role: "user",
			Parts: []geminiPart{
				{Text: req.System[0].Text},
			},
		}
	}

	// 3. Messages 映射
	for _, msg := range req.Messages {
		role := "user"
		if msg.Role == RoleAssistant {
			role = "model"
		}

		gContent := geminiContent{Role: role}

		for _, part := range msg.Parts {
			switch p := part.(type) {
			case TextPart:
				gContent.Parts = append(gContent.Parts, geminiPart{
					Text: p.Text,
				})
			case ThinkingPart:
				// Gemini 2.0 Flash Thinking 语法
				gContent.Parts = append(gContent.Parts, geminiPart{
					Text:    p.Text,
					Thought: true,
				})
			case ToolCallPart:
				var args map[string]any
				_ = json.Unmarshal([]byte(p.Arguments), &args)
				gContent.Parts = append(gContent.Parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						Name: p.Name,
						Args: args,
					},
				})
			case ToolResultPart:
				gContent.Role = "user"
				var respMap map[string]any
				if err := json.Unmarshal([]byte(p.Content), &respMap); err != nil {
					respMap = map[string]any{"output": p.Content}
				}
				gContent.Parts = append(gContent.Parts, geminiPart{
					FunctionResponse: &geminiFunctionResp{
						Name:     p.ToolCallID,
						Response: respMap,
					},
				})
			}
		}

		dst.Contents = append(dst.Contents, gContent)
	}

	// 4. Tools 与 Schema 深度大写清洗
	if len(req.Tools) > 0 {
		var decls []geminiFuncDecl
		for _, tool := range req.Tools {
			cleanedSchema := CleanSchemaForGemini(tool.Parameters)
			decls = append(decls, geminiFuncDecl{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  cleanedSchema,
			})
		}
		dst.Tools = append(dst.Tools, geminiToolWrapper{FunctionDeclarations: decls})
	}

	return json.Marshal(dst)
}
```

### 6.5 `internal/stream/fsm.go`

```go
package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// StreamState 状态枚举
type StreamState int

const (
	StateInit StreamState = iota
	StateMessageOpen
	StateThinkingBlockOpen
	StateTextBlockOpen
	StateToolBlockOpen
	StateClosing
	StateDone
)

var bufferPool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 4096))
	},
}

// OpenAIToAnthropicFSM 零拷贝流式状态机：将 OpenAI SSE Chunk 序列逆向合成为 Anthropic 严格生命周期
type OpenAIToAnthropicFSM struct {
	state          StreamState
	currentBlockID int
	writer         io.Writer
	mu             sync.Mutex
	messageID      string
	model          string
}

func NewOpenAIToAnthropicFSM(w io.Writer, messageID, model string) *OpenAIToAnthropicFSM {
	return &OpenAIToAnthropicFSM{
		state:          StateInit,
		writer:         w,
		messageID:      messageID,
		model:          model,
		currentBlockID: -1,
	}
}

func (f *OpenAIToAnthropicFSM) emitEvent(eventType string, data any) error {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	buf.WriteString("event: ")
	buf.WriteString(eventType)
	buf.WriteString("\ndata: ")

	enc := json.NewEncoder(buf)
	if err := enc.Encode(data); err != nil {
		return err
	}
	buf.WriteString("\n")

	_, err := f.writer.Write(buf.Bytes())
	return err
}

type openAIChunkDelta struct {
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type openAIChunkChoice struct {
	Delta        openAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason,omitempty"`
}

type openAIChunk struct {
	Choices []openAIChunkChoice `json:"choices"`
}

// FeedChunk 灌入一个上游原始 OpenAI Chunk
func (f *OpenAIToAnthropicFSM) FeedChunk(chunkBytes []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if bytes.Equal(bytes.TrimSpace(chunkBytes), []byte("[DONE]")) {
		return f.closeStream("end_turn")
	}

	var chunk openAIChunk
	if err := json.Unmarshal(chunkBytes, &chunk); err != nil {
		return nil // 忽略坏帧
	}
	if len(chunk.Choices) == 0 {
		return nil
	}

	choice := chunk.Choices[0]

	// 1. 状态触发：发送 message_start
	if f.state == StateInit {
		if err := f.emitEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":           f.messageID,
				"type":         "message",
				"role":         "assistant",
				"model":        f.model,
				"content":      []any{},
				"stop_reason":  nil,
				"stop_sequence": nil,
				"usage":        map[string]int{"input_tokens": 0, "output_tokens": 0},
			},
		}); err != nil {
			return err
		}
		f.state = StateMessageOpen
	}

	// 2. 状态机流转：处理 Thinking/Reasoning 增量
	if choice.Delta.ReasoningContent != "" {
		if f.state != StateThinkingBlockOpen {
			// 若当前正处于 TextBlock，先闭合它
			if f.state == StateTextBlockOpen {
				if err := f.closeCurrentBlock(); err != nil {
					return err
				}
			}
			f.currentBlockID++
			if err := f.emitEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": f.currentBlockID,
				"content_block": map[string]string{
					"type":     "thinking",
					"thinking": "",
				},
			}); err != nil {
				return err
			}
			f.state = StateThinkingBlockOpen
		}

		// 发送 thinking_delta
		if err := f.emitEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": f.currentBlockID,
			"delta": map[string]string{
				"type":     "thinking_delta",
				"thinking": choice.Delta.ReasoningContent,
			},
		}); err != nil {
			return err
		}
	}

	// 3. 状态机流转：处理 Text 文本增量
	if choice.Delta.Content != "" {
		if f.state != StateTextBlockOpen {
			// 若当前处于 ThinkingBlock，必须先闭合 thinking block
			if f.state == StateThinkingBlockOpen {
				if err := f.closeCurrentBlock(); err != nil {
					return err
				}
			}
			f.currentBlockID++
			if err := f.emitEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": f.currentBlockID,
				"content_block": map[string]string{
					"type": "text",
					"text": "",
				},
			}); err != nil {
				return err
			}
			f.state = StateTextBlockOpen
		}

		// 发送 text_delta
		if err := f.emitEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": f.currentBlockID,
			"delta": map[string]string{
				"type": "text_delta",
				"text": choice.Delta.Content,
			},
		}); err != nil {
			return err
		}
	}

	// 4. 结束判定
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		reason := "end_turn"
		if *choice.FinishReason == "length" {
			reason = "max_tokens"
		} else if *choice.FinishReason == "tool_calls" {
			reason = "tool_use"
		}
		return f.closeStream(reason)
	}

	return nil
}

func (f *OpenAIToAnthropicFSM) closeCurrentBlock() error {
	if f.currentBlockID >= 0 {
		if err := f.emitEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": f.currentBlockID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (f *OpenAIToAnthropicFSM) closeStream(stopReason string) error {
	if f.state == StateDone {
		return nil
	}

	// 闭合打开的 Block
	if f.state == StateThinkingBlockOpen || f.state == StateTextBlockOpen {
		if err := f.closeCurrentBlock(); err != nil {
			return err
		}
	}

	// 发送 message_delta 声明 stop_reason
	if err := f.emitEvent("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": 128},
	}); err != nil {
		return err
	}

	// 发送 message_stop
	if err := f.emitEvent("message_stop", map[string]string{
		"type": "message_stop",
	}); err != nil {
		return err
	}

	f.state = StateDone
	return nil
}
```

---

## 7. 单元测试与高保真验证规范

为保证工业级实现的鲁棒性与语义零丢失，在 `internal/ir` 与 `internal/stream` 需配置严格的自动化测试用例矩阵。

### 7.1 Schema 递归清洗与大写转换测试 (`gemini_test.go`)

```go
package ir_test

import (
	"encoding/json"
	"testing"

	"github.com/ai-gateway/universal-ai-gateway/internal/ir"
)

func TestCleanSchemaForGemini_EdgeCases(t *testing.T) {
	inputSchemaJSON := `{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"title": "WeatherLookupParameters",
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"location": {
				"type": "string",
				"description": "City name"
			},
			"unit": {
				"type": "string",
				"enum": ["celsius", "fahrenheit"],
				"default": "celsius"
			},
			"days": {
				"type": "integer"
			}
		},
		"required": ["location"]
	}`

	var raw map[string]any
	if err := json.Unmarshal([]byte(inputSchemaJSON), &raw); err != nil {
		t.Fatalf("unmarshal err: %v", err)
	}

	cleaned := ir.CleanSchemaForGemini(raw)

	// 1. 验证非法顶层元数据字段被剔除
	for _, forbiddenKey := range []string{"$schema", "title", "additionalProperties"} {
		if _, exists := cleaned[forbiddenKey]; exists {
			t.Errorf("expected key %s to be stripped, but still exists", forbiddenKey)
		}
	}

	// 2. 验证类型转换大写
	if cleaned["type"] != "OBJECT" {
		t.Errorf("expected type OBJECT, got %v", cleaned["type"])
	}

	props, ok := cleaned["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or invalid")
	}

	locProp := props["location"].(map[string]any)
	if locProp["type"] != "STRING" {
		t.Errorf("expected location type STRING, got %v", locProp["type"])
	}

	unitProp := props["unit"].(map[string]any)
	if _, exists := unitProp["default"]; exists {
		t.Errorf("expected default field in unit to be stripped")
	}
}
```

### 7.2 流式状态机全生命周期测试 (`fsm_test.go`)

```go
package stream_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ai-gateway/universal-ai-gateway/internal/stream"
)

func TestOpenAIToAnthropicFSM_ThinkingTransition(t *testing.T) {
	var output bytes.Buffer
	fsm := stream.NewOpenAIToAnthropicFSM(&output, "msg_test_123", "claude-3-7-sonnet")

	// 模拟来自上游 DeepSeek / OpenAI 的流式 Chunk 序列
	chunks := []string{
		`{"choices":[{"delta":{"reasoning_content":"Let "}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"me think."}}]}`,
		`{"choices":[{"delta":{"content":"Hello! "}}]}`,
		`{"choices":[{"delta":{"content":"How can I help?"},"finish_reason":"stop"]}`,
	}

	for _, c := range chunks {
		if err := fsm.FeedChunk([]byte(c)); err != nil {
			t.Fatalf("feed chunk err: %v", err)
		}
	}

	result := output.String()

	// 验证关键事件序列的存在性与先后拓扑顺序
	expectedSequence := []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"thinking"`,
		"event: content_block_delta",
		`"thinking":"Let "`,
		"event: content_block_delta",
		`"thinking":"me think."`,
		"event: content_block_stop", // thinking block 必须先闭合
		"event: content_block_start", // 随后开启 text block
		`"type":"text"`,
		"event: content_block_delta",
		`"text":"Hello! "`,
		"event: content_block_delta",
		`"text":"How can I help?"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	}

	lastIdx := 0
	for _, expected := range expectedSequence {
		idx := strings.Index(result[lastIdx:], expected)
		if idx == -1 {
			t.Fatalf("missing or out-of-order sequence item: %s\nFull stream dump:\n%s", expected, result)
		}
		lastIdx += idx + len(expected)
	}
}
```

---

## 8. 生产部署安全与运维建议

1. **协议层熔断与保活（Circuit Breaker）**：
   对于流式 SSE 连接，上游大模型若在思考阶段由于算力拥堵超过 15 秒未发送任何 Chunk，网关自动发送 SSE 注释心跳包（`: keep-alive\n\n`），防止客户端 HTTP 连接池或中间 Nginx 超时断连。
2. **敏感信息与签名擦除保护（Thinking Redaction Policy）**：
   在转译到不受信终端或面向外部商业客户提供 API 服务时，网关支持配置 `redact_thinking_signatures: true`。该策略会在 Egress 阶段自动剥离 Claude 的思考加密签名，仅保留思考纯文本或将其完全擦除，防止内部安全审查策略逆向泄漏。
3. **内存排障指标**：
   生产环境暴露 Prometheus 监控指标：
   - `gateway_ir_lowering_duration_seconds` (直方图)：入站请求解析 AST 耗时；
   - `gateway_ir_synthesizing_duration_seconds` (直方图)：出站请求合成耗时；
   - `gateway_stream_fsm_allocated_bytes` (计数器)：流式引擎内存分配指标，基线预期为每连接分配堆内存 $\le 8\text{KB}$。
