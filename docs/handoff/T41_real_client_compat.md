# T41 真实客户端兼容性实测

- **类型**：调研 + 回归用例（**不修改转译行为**）
- **前置**：无
- **需要用户审批**：否
- **必读**：
  - [`spec/10_ir_translation.md`](../../spec/10_ir_translation.md) §1、§3、§6.1、§8.1、§8.2：各转译方向接受、丢弃、拒绝哪些字段
  - [`docs/DEPLOYMENT.md`](../DEPLOYMENT.md) §1 快速开始、§6.1 客户端接入
  - [`gateway.example.yaml`](../../gateway.example.yaml)
  - [`internal/translate/chat_lower.go`](../../internal/translate/chat_lower.go)、[`anthropic_lower.go`](../../internal/translate/anthropic_lower.go)、[`responses_lower.go`](../../internal/translate/responses_lower.go)：看各自的顶层字段表（`chatTopLevel`、`anthropicTopLevel`、`responsesTopLevel`）

## 1. 背景与目标

转译层（M2）对请求字段的策略很严格：**规格没列出的字段一律 400**（见代码中的 `SPEC-GAP(T20)`、`SPEC-GAP(T23)`、`SPEC-GAP(T25)`）。
这样做是保守的，但真实客户端会发送很多规格没有列出的字段，而且客户端升级时还会新增字段。目前**没有用任何真实客户端测过**。

本卡要回答一个问题：**Codex CLI 和 Claude Code 实际发送了什么，哪些请求会被转译层拒绝。**
产出是一份报告和一组脱敏后的回归用例，供 T42 决定字段策略。**本卡不修改任何转译行为。**

## 2. 测试矩阵

| 路径 | 客户端 | 客户端协议 | 上游池 | 示例上游 |
|:--|:--|:--|:--|:--|
| P1 | Claude Code | `anthropic` | `openai_chat` | DeepSeek（`deepseek-chat`） |
| P2 | Codex CLI | `openai_responses` | `anthropic` | Claude（`claude-sonnet-4-5`） |
| P3 | Codex CLI | `openai_responses` | `openai_chat` | DeepSeek |
| P4 | 任意 OpenAI Chat 客户端（OpenAI SDK、Cursor、Cherry Studio 等，选一个） | `openai_chat` | `anthropic` | Claude |
| 对照 | Claude Code → `anthropic` 池；Codex CLI → `openai_responses` 池 | 同协议透传 | — | 用于确认问题出在转译层而不是网关其他部分 |

每条路径至少跑以下场景，记录每个场景**第一次失败**的请求：

| 编号 | 场景 | 重点观察 |
|:--|:--|:--|
| S1 | 一句话问答（非流式，如果客户端支持） | 基本字段 |
| S2 | 同样的问题，流式 | 流事件顺序、结尾、用量 |
| S3 | 让它读一个文件并总结（触发一次工具调用 + 工具结果回填） | 工具定义的字段、tool_choice、工具结果格式 |
| S4 | 让它同时读两个文件（并行工具调用） | 多个工具调用的索引 |
| S5 | 连续 5 轮以上的对话 | 历史消息里带回的字段（思考块、加密推理、缓存标记） |
| S6 | 开启思考 / 推理（Claude Code 的 thinking；Codex 的 `reasoning.effort`） | 思考相关字段是否被正确丢弃 |
| S7 | 粘贴一张图片（客户端支持时） | 图片格式 |
| S8 | 中途按 Esc / Ctrl+C 取消 | 网关是否正常结束、账本是否释放或结算（看审计与 usage） |
| S9 | Codex CLI：默认工具集（`shell`、`apply_patch`、`web_search` 等） | 内置工具会被 400 拒绝（§8.2），记录具体是哪些 |

## 3. 环境搭建

### 3.1 网关

1. 按 DEPLOYMENT.md §1 在本机启动网关（`data_listen: 127.0.0.1:8080`）。
2. 配置里至少放三个池：`anthropic`（Claude）、`openai_chat`（DeepSeek）、`openai_responses`（OpenAI，用作对照，可选）。
   为每个池在 `models` 里登记服务，例如 `claude-sonnet-4-5`、`deepseek-chat`。
3. 用管理 API 为每个客户端创建 Token，并设置额度（例如 5 美元）与 `allowed_models`。
4. 上游 Key 用**你自己的测试 Key**，通过管理 API 加入。**不要**写进任何会被提交的文件。

### 3.2 记录代理（放在仓库之外，不要提交）

网关按设计**不记录请求正文**（spec/01 「明确不做」）。要看到客户端发的原始请求，在客户端和网关之间放一个只在本机运行的记录代理。
在仓库**外**新建一个目录，写一个最小的 Go 程序：

```go
// recorder/main.go —— 本机调试工具，切勿提交到仓库
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
)

func main() {
	target, _ := url.Parse("http://127.0.0.1:8080") // 网关的数据面地址
	dir := "captures"
	_ = os.MkdirAll(dir, 0o700)
	var seq atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // 流式响应立即转发
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.StatusCode >= 400 { // 只保存失败响应的正文，便于对照
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body = io.NopCloser(bytes.NewReader(b))
			n := resp.Request.Header.Get("X-Rec-Seq")
			_ = os.WriteFile(filepath.Join(dir, n+".resp.json"), b, 0o600)
		}
		return nil
	}
	http.ListenAndServe("127.0.0.1:9090", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		r.Body = io.NopCloser(bytes.NewReader(b))
		n := fmt.Sprintf("%04d", seq.Add(1))
		r.Header.Set("X-Rec-Seq", n)
		_ = os.WriteFile(filepath.Join(dir, n+"."+filepath.Base(r.URL.Path)+".req.json"), b, 0o600)
		proxy.ServeHTTP(w, r)
	}))
}
```

把客户端指向 `http://127.0.0.1:9090`，而不是 8080：

| 客户端 | 配置 |
|:--|:--|
| Claude Code | `ANTHROPIC_BASE_URL=http://127.0.0.1:9090`、`ANTHROPIC_AUTH_TOKEN=uai_live_...`、`ANTHROPIC_MODEL=deepseek-chat`（或在 `/model` 里选网关登记的服务名） |
| Codex CLI | `~/.codex/config.toml` 里定义 provider：`base_url = "http://127.0.0.1:9090/v1"`、`env_key = "UAIG_TOKEN"`、`wire_api = "responses"`，模型写网关登记的服务名 |

`captures/` 里会出现客户端的完整提示词、你的代码和文件内容。**它只能留在本机，用完删除。**

## 4. 要做的事

1. 搭好 §3 的环境，逐条跑 §2 的矩阵。
2. 对每一个失败（HTTP 4xx/5xx，或流中出现错误事件）：
   1. 从网关返回的错误消息中找出触发拒绝的字段路径（转译层的错误消息都会写出字段名，例如 `top_k is not supported when translating to another protocol`）。
   2. 在对应的 `captures/*.req.json` 里确认这个字段的实际取值。
   3. 判断它属于哪一类：
      - **A 可以安全丢弃**：只影响客户端提示或计费标签，对生成结果没有语义影响（例如某些 metadata 类字段）。
      - **B 应该映射**：目标上游有等价参数，规格漏写了。
      - **C 应该继续拒绝**：丢弃会改变生成结果或工具调用语义（例如强制 JSON 输出、内置工具）。
      - **D 不确定**：写清楚不确定的原因。
   4. 临时绕过这个字段（例如在客户端配置里关掉对应功能），继续往下测，以便发现后面的失败。如果绕不过，就记录「阻塞」。
3. 场景都能通过的路径，也要检查结果是否**正确**：
   - 工具调用确实被执行，结果被模型读到。
   - 流式输出没有重复或缺字。
   - 管理 API `GET /admin/api/usage` 里的用量与上游控制台大致一致。
4. 把每个失败请求**脱敏**后做成回归用例（§5）。
5. 写报告（§6）。

## 5. 回归用例

### 5.1 目录

```
internal/translate/testdata/real_clients/
  claude_code/<场景>_<简述>/input.json      # Anthropic 请求体，脱敏后
  claude_code/<场景>_<简述>/expect.txt      # 当前行为：一行，"ok" 或 "error: <错误消息中的关键子串>"
  codex/<场景>_<简述>/input.json             # Responses 请求体，脱敏后
  codex/<场景>_<简述>/expect.txt
```

能通过的代表性请求也要收录至少 3 个（`expect.txt` 为 `ok`），用来防止 T42 改坏现有行为。

### 5.2 脱敏规则（必须全部做到）

- 所有消息正文、system / instructions 文本、工具结果内容、工具调用参数的**值**：替换为 `"<redacted>"`。工具调用参数要保持合法的 JSON 对象，例如 `{"path":"<redacted>"}`。
- 工具定义：保留 `name`、`type`、`input_schema` / `parameters` 的**结构**（键名与类型），`description` 替换为 `"<redacted>"`。
- 图片 base64：替换为一个 1×1 PNG 的 base64（`iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=`）。
- 加密推理（`encrypted_content`）、思考签名（`signature`）、`redacted_thinking.data`：替换为 `"<redacted>"`。
- 删除任何看起来像 Token、Key、邮箱、用户名、本机路径、仓库名的值。
- **字段名和结构不要动**：兼容性问题正是出在字段名上。

做完后用 `grep -ri "sk-\|uai_live_\|@\|C:\\\\\|/home/\|/Users/" internal/translate/testdata/real_clients/` 自查，结果应为空。

### 5.3 测试

新增 `internal/translate/real_clients_test.go`，函数名 `TestRealClientFixtures`：

- 遍历 `testdata/real_clients/claude_code/*`，用 `lowerAnthropic` 解析；遍历 `testdata/real_clients/codex/*`，用 `lowerOpenAIResponses` 解析。
- `expect.txt` 为 `ok` 时：解析必须成功，并且还要能合成目标上游的请求体。claude_code 合成 `synthesizeOpenAIChat`；codex 分别合成 `synthesizeAnthropic` 和 `synthesizeOpenAIChat`。
- `expect.txt` 为 `error: xxx` 时：必须返回 `invalid_request`，并且错误消息包含 `xxx`。
- 这个测试**记录当前行为**。T42 修改字段策略时，会把相应用例的 `expect.txt` 从 `error:` 改为 `ok`。

## 6. 报告

新增 `docs/handoff/reports/T41_compat_report.md`：

1. 环境：客户端名称与**确切版本号**（`claude --version`、`codex --version`），网关提交号，上游模型。
2. 矩阵结果表：路径 × 场景，填「通过 / 失败 / 阻塞 / 不适用」。
3. 失败清单，每条一行：

   | # | 路径 | 场景 | 字段路径 | 实际取值（脱敏） | 网关错误消息 | 分类 A/B/C/D | 建议 | 用例目录 |
   |:--|:--|:--|:--|:--|:--|:--|:--|:--|

4. 非 400 类的问题，例如流式输出错乱、用量不一致、取消后账本没有释放。每条都要附复现步骤。
5. 总结：每条路径「现在能不能用」，以及要做到能用最少要改哪些字段。

## 7. 涉及文件

- 新增：`internal/translate/testdata/real_clients/**`、`internal/translate/real_clients_test.go`、`docs/handoff/reports/T41_compat_report.md`
- **不要改**：`internal/translate` 下任何非测试文件、任何已有黄金文件、`spec/`

## 8. 验收标准

- [ ] §2 的矩阵每格都有结论（通过 / 失败 / 阻塞 / 不适用）。
- [ ] 每个失败都有对应的脱敏用例，`TestRealClientFixtures` 通过。
- [ ] 至少 3 个 `ok` 用例。
- [ ] §5.2 的自查 grep 为空；审查方会抽查用例里有没有残留的提示词或代码。
- [ ] 报告按 §6 写完，客户端版本号准确。
- [ ] `go run ./tools/check` 全绿，转译层的非测试代码没有任何改动。

## 9. 建议提交拆分

1. `test(translate): T41 新增真实客户端请求回归用例`
2. `docs(handoff): T41 真实客户端兼容性报告`
