---
name: use-gateway
description: 通过用户自建的 Universal AI Gateway（环境变量 UAIG_URL、UAIG_TOKEN，Token 以 uai_live_ 开头）调用大模型、Tavily 搜索、MinerU 文档解析和其他 REST API。当项目要调用任何 LLM（OpenAI / Anthropic / Gemini SDK）、做联网搜索、解析 PDF、或调用第三方 API，而用户说过「用网关」「走 uaig」，或环境中有 UAIG_URL / UAIG_TOKEN 时使用。不用于网关本身的开发与运维。
---

# 通过 Universal AI Gateway 调用 AI 与 API

用户有一个自托管网关：项目代码只和它通信，它持有上游 Key、换 Key 重试、计费，并让同一个模型能用任何主流 SDK 调用。完整说明在网关仓库的 `docs/AI_QUICKSTART.md`；本技能是精简版。

## 硬规则

1. 地址和 Token 只从环境变量 `UAIG_URL`（不带 `/v1`）、`UAIG_TOKEN` 读取（或项目已有的 `.env` 机制）。**不写进代码、提交、日志、输出**，也不要回显它的值。
2. 缺少任一个时停下来问用户；不要去找、也不要使用上游厂商的 Key 或地址（`api.openai.com`、`api.anthropic.com` 等）。
3. `model` 只用网关列出的服务名（第 1 步），不要凭记忆写厂商模型名。

## 步骤

### 1. 查可用的服务名

```bash
curl -s "$UAIG_URL/v1/models" -H "Authorization: Bearer $UAIG_TOKEN"
```

返回 `{"data":[{"id":"<服务名>"}, ...]}`。只列出这个 Token 有权限的服务。失败时：401 → Token 不对，问用户；连不上 → 确认网关在运行、地址对不对。

### 2. 选择调用方式（沿用项目已有的 SDK）

| 用途 | 写法 |
|:--|:--|
| OpenAI SDK（Chat / Responses） | `base_url = UAIG_URL + "/v1"`，`api_key = UAIG_TOKEN` |
| Anthropic SDK | `base_url = UAIG_URL`（不带 `/v1`），`api_key = UAIG_TOKEN` |
| Gemini（REST / SDK） | `POST {UAIG_URL}/v1beta/models/{服务名}:generateContent`（或 `:streamGenerateContent`），头 `x-goog-api-key: UAIG_TOKEN` |
| Tavily 搜索 | `POST {UAIG_URL}/search`（`/extract`、`/crawl`），请求体同 Tavily 官方，不需要 `api_key` |
| MinerU 解析 PDF → Markdown | `POST {UAIG_URL}/v1/parse` JSON `{"url": "..."}`；本地文件 `POST {UAIG_URL}/v1/parse/file?name=a.pdf`，请求体是文件，带 `Content-Length` |
| 其他 REST API | `{UAIG_URL}/rest/{服务名}/{上游路径}`，其余按该 API 官方文档；查询类 POST 用 `/rest-idem/` 以便自动重试 |

任何 SDK 都能调用任何大模型服务（网关转换协议，支持流式与工具调用）。转换时不支持 OpenAI Responses 的服务端状态（`previous_response_id`、`conversation`、`background`）：每次发送完整对话。

Python 示例：

```python
import os
from openai import OpenAI

client = OpenAI(base_url=os.environ["UAIG_URL"] + "/v1", api_key=os.environ["UAIG_TOKEN"])
reply = client.chat.completions.create(model="<服务名>", messages=[{"role": "user", "content": "hi"}])
```

上游需要的自定义请求头用 `X-UAIG-Forward-<名字>` 传。多轮对话可加 `X-UAIG-Session: <会话 ID>` 提高缓存命中（可选）。

### 3. 错误处理

| 状态 | 做法 |
|:--|:--|
| 400 `invalid_request`（含 `unknown model`） | 按 `message` 修正，不重试 |
| 401 / 402 / 403 | Token 无效 / 额度用完 / 无权限：告诉用户，不重试 |
| 429 | 按 `Retry-After` 等待后重试 |
| 502 / 503 / 504 | 网关已换 Key 重试过；客户端最多再退避重试 2–3 次 |

### 4. 本地文件交给接口

接口要的是文件本身时：`gateway files upload <路径>` 得到 `file_…`，之后工具参数填这个 ID，或 HTTP 请求体留空并加头 `X-UAIG-File: file_…`。需要网关开启 `files.enabled`。

### 5. 自己（AI 助手）要用网关能力时

- MCP：`claude mcp add --transport http uaig "$UAIG_URL/mcp" --header "Authorization: Bearer $UAIG_TOKEN"`。工具多时先 `gateway_search_tools`，再 `gateway_describe_tool`，再 `gateway_call_tool`。
- 命令行（有 `gateway` 可执行文件时）：`gateway tools`、`gateway call <工具> --args '<JSON>'`，例如 `gateway call ask_model --args '{"model":"<服务名>","input":"..."}'`。

## 完成前检查

- 代码、配置样例、测试、日志里没有 Token 明文，也没有上游厂商的 Key 或地址。
- 服务名来自 `/v1/models`；OpenAI 系 base URL 带 `/v1`，Anthropic 与 Gemini 不带。
- 用一次真实的小请求验证（比如问一句 "ping"），并把用到的服务名告诉用户（调用会计费）。
