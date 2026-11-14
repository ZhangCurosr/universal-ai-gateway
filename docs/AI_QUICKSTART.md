# 通过 Universal AI Gateway 调用 AI 与 API（给 AI 编程助手的接入说明）

> 读者：在**其他项目**里写代码的 AI 助手，之前不了解这个网关。读完这一页就能正确调用。
> 运维、配置、导入 Key 等管理操作不在本页，见 [DEPLOYMENT.md](./DEPLOYMENT.md)。
> 一条命令配好你用的 AI CLI（Claude Code、Codex、OpenCode、Pi）并安装同内容的技能 `use-gateway`：`gateway connect <cli> --write`（见 §10）。

## 1. 它是什么

一个自托管的网关。项目代码**只和网关通信**，网关替你：

- 持有各家的上游 API Key（你看不到，也不需要）；
- 在多把 Key 之间调度、限流时自动换 Key、按用量计费；
- 让同一个模型能用 Anthropic、OpenAI Chat、OpenAI Responses、Gemini 任一种 SDK 调用（自动转换协议）；
- 把搜索（Tavily）、文档解析（MinerU）和其他 REST API 也放在同一个地址、同一个 Token 后面。

## 2. 你需要的两样东西

| 环境变量 | 含义 | 例子 |
|:--|:--|:--|
| `UAIG_URL` | 网关数据面地址（不带 `/v1`） | `http://127.0.0.1:8080` |
| `UAIG_TOKEN` | 客户端 Token，以 `uai_live_` 开头 | `uai_live_…` |

规则：

1. **只从环境变量（或项目的 `.env`）读取这两个值**，不要写进代码、提交、日志或输出。
2. 缺少时**向用户要**，不要自己猜，也不要去找上游厂商的 Key。Token 由网关管理员创建。
3. 不要在项目里直接调用 `api.anthropic.com`、`api.openai.com` 等上游地址；全部走网关。

## 3. 先看有什么可用

```bash
curl -s "$UAIG_URL/v1/models" -H "Authorization: Bearer $UAIG_TOKEN"
```

返回这个 Token 能用的**服务名**列表（OpenAI 格式：`{"data":[{"id":"…"}]}`）。下面所有的 `model` 都填这里的 `id`，它是网关配置里的名字，不一定等于厂商的原始模型名。

装了网关命令行时，`gateway tools` 列出可作为工具调用的接口（见 §6）。

## 4. 调用大模型

认证头三选一，都填 `UAIG_TOKEN`：`Authorization: Bearer …`、`x-api-key: …`、`x-goog-api-key: …`。用哪个 SDK 就按它的习惯来。

| SDK / 协议 | base URL | 端点 |
|:--|:--|:--|
| OpenAI Chat Completions | `$UAIG_URL/v1` | `POST /v1/chat/completions` |
| OpenAI Responses | `$UAIG_URL/v1` | `POST /v1/responses` |
| Anthropic Messages | `$UAIG_URL`（**不带** `/v1`） | `POST /v1/messages` |
| Google Gemini | `$UAIG_URL` | `POST /v1beta/models/{model}:generateContent`、`:streamGenerateContent` |

**任何一种 SDK 都能调用 `/v1/models` 里的任何大模型**，网关负责转换；流式（SSE）、工具调用（function calling）都支持。选项目里已经在用的 SDK 即可。

Python（OpenAI SDK）：

```python
import os
from openai import OpenAI

client = OpenAI(base_url=os.environ["UAIG_URL"] + "/v1", api_key=os.environ["UAIG_TOKEN"])
r = client.chat.completions.create(model="<服务名>", messages=[{"role": "user", "content": "你好"}])
print(r.choices[0].message.content)
```

Python（Anthropic SDK）：

```python
import os
from anthropic import Anthropic

client = Anthropic(base_url=os.environ["UAIG_URL"], api_key=os.environ["UAIG_TOKEN"])
r = client.messages.create(model="<服务名>", max_tokens=1024, messages=[{"role": "user", "content": "你好"}])
print(r.content[0].text)
```

TypeScript（OpenAI SDK）：

```ts
import OpenAI from "openai";
const client = new OpenAI({ baseURL: process.env.UAIG_URL + "/v1", apiKey: process.env.UAIG_TOKEN });
const r = await client.chat.completions.create({ model: "<服务名>", messages: [{ role: "user", content: "你好" }] });
```

注意：

- 经过协议转换时（比如用 OpenAI SDK 调 Claude 模型），OpenAI Responses 的服务端状态不可用：不要用 `previous_response_id`、`conversation`、`background`，每次发送完整对话。
- 多轮对话想尽量落在同一把上游 Key 上（提高缓存命中），可以加请求头 `X-UAIG-Session: <会话 ID>`（≤ 128 字节）。可选。

## 5. 调用搜索、文档解析和其他 REST API

| 服务 | 怎么调 |
|:--|:--|
| Tavily 搜索 | `POST $UAIG_URL/search`、`/extract`、`/crawl`，请求体与 Tavily 官方 API 相同；不需要 `api_key`（有也会被网关删掉） |
| MinerU 文档解析 | 同步返回 Markdown：`POST $UAIG_URL/v1/parse`，JSON `{"url": "https://…/a.pdf"}`；本地文件 `POST $UAIG_URL/v1/parse/file?name=a.pdf`，请求体是文件本身（必须带 `Content-Length`）。异步：`/v1/tasks`、`/v1/tasks/file`，再 `GET /v1/tasks/{id}`、`/v1/tasks/{id}/result` |
| 其他接入的 REST API | `$UAIG_URL/rest/{服务名}/{上游路径}`，方法、查询串、请求体原样转发，网关替你加上游 Key |

- `{服务名}` 同样来自 `/v1/models`；`{上游路径}` 按该 API 官方文档写。
- `/rest/` 下的 `POST` / `PATCH` 失败不重试（避免重复写入）。确定接口**没有副作用**时（查询类 POST），改用 `/rest-idem/{服务名}/…`，失败会自动换 Key 重试。
- 上游要求的自定义请求头用 `X-UAIG-Forward-<头名>: <值>` 传，例如 `X-UAIG-Forward-X-Api-Version: 2`。认证类头会被丢弃。
- 二进制响应（文件下载）原样返回。

## 6. 让 AI 工具直接用（MCP / 命令行）

如果你（AI 助手）自己要用网关的能力，而不是写进项目代码：

```bash
# 远程 MCP（推荐）
claude mcp add --transport http uaig "$UAIG_URL/mcp" --header "Authorization: Bearer $UAIG_TOKEN"
```

命令行（需要 `gateway` 可执行文件，读取 `UAIG_URL` / `UAIG_TOKEN`）：

```bash
gateway tools                                             # 列出可用工具
gateway call tavily_search --args '{"query":"..."}'       # 调用一个工具，结果文本写到 stdout
gateway call ask_model --args '{"model":"<服务名>","input":"..."}'   # 问另一个模型
```

工具很多时 MCP 只列出 `gateway_search_tools`、`gateway_describe_tool`、`gateway_call_tool` 三个：先搜索，再查看参数，再调用。

## 7. 本地文件交给接口（文件中转）

接口需要的是文件本身、而你手里只有本地路径时（例如让 MinerU 解析本机 PDF）：

```bash
gateway files upload ./report.pdf --ttl 2h    # 输出 file_… ID
```

之后：

- 工具声明了文件参数的（如 `mineru_parse_file`），参数填 `file_id`；
- 直接 HTTP：请求体留空，加头 `X-UAIG-File: file_…`（需要文件名参数时再加 `X-UAIG-File-Name-Query: name`）。

本地 stdio MCP（`gateway mcp --upload-dir <目录>`）会提供 `gateway_upload_file {path}`，只能上传用户允许的目录里的文件。需要网关开启 `files.enabled`，否则返回 400。

## 8. 错误与重试

错误体是所用协议的标准格式（OpenAI 端点返回 `{"error":{"message","type","code"}}`，Anthropic 端点返回 Anthropic 格式）。

| 状态码 | `code` | 含义与做法 |
|:--|:--|:--|
| 400 | `invalid_request` | 请求有误（包括 `unknown model`：服务名不存在），看 `message` 修正，不要重试 |
| 401 | `unauthenticated` | Token 缺失或无效：检查 `UAIG_TOKEN`，向用户确认 |
| 402 | `insufficient_quota` | 该 Token 的额度用完：告诉用户，不要重试 |
| 403 | `permission_denied` | 该 Token 不能用这个服务：换 `/v1/models` 里有的，或请用户开权限 |
| 404 | `not_found` | 路径不存在，或文件 ID 不存在 / 已过期 |
| 429 | `rate_limited` | 限流：按 `Retry-After` 等待后重试 |
| 502 / 503 / 504 | `upstream_error` / `upstream_unavailable`、`overloaded` / `timeout` | 网关已经换 Key 重试过；稍后指数退避重试（有 `Retry-After` 就按它） |

网关已经在多把 Key 之间重试，**客户端只需要很少的重试**（2–3 次、指数退避），不要并发轰炸。

## 9. 检查清单

- [ ] `UAIG_URL`、`UAIG_TOKEN` 从环境变量读取，代码和日志里没有 Token。
- [ ] `model` / `{服务名}` 来自 `/v1/models`。
- [ ] base URL：OpenAI 系带 `/v1`，Anthropic 与 Gemini 不带。
- [ ] 项目里没有任何上游厂商的 Key 或地址。
- [ ] 401 / 402 / 403 不重试，429 / 5xx 退避重试。

## 10. 把网关配成各 AI CLI 的模型后端（`gateway connect`）

在要使用网关的项目目录里运行（不加 `--write` 只打印，不改任何文件）：

```bash
gateway connect codex --write              # 或 claude / opencode / pi；--scope user 写到用户目录
```

它会写好该 CLI 的模型后端与 MCP 配置，并把 `use-gateway` 技能装到该 CLI 读取的技能目录。**Token 不会写进任何文件**，配置里只引用环境变量 `UAIG_TOKEN`，请自己设置一次（Windows：`setx UAIG_TOKEN "uai_live_..."`，然后重开终端）。服务名取自网关（能当大模型用的服务）。

| CLI | 写入的文件 | 内容 |
|:--|:--|:--|
| Claude Code | `.mcp.json`；技能 `.claude/skills/use-gateway/` | MCP `uaig`（`Authorization: Bearer ${UAIG_TOKEN}`）；模型后端用环境变量 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN`（命令会打印设置方法，不写进 `settings.json`） |
| Codex | `.codex/config.toml`（标记块 `# >>> uaig …`）；技能 `.agents/skills/use-gateway/` | `[model_providers.uaig]`（`base_url = "<网关>/v1"`、`env_key = "UAIG_TOKEN"`、`wire_api = "responses"`）与 `[mcp_servers.uaig]`（`bearer_token_env_var = "UAIG_TOKEN"`）；`model_provider = "uaig"` 需要放在文件开头，或用 `codex -c model_provider=uaig -m <服务名>` |
| OpenCode | `opencode.json`；技能 `.agents/skills/use-gateway/` | provider `uaig`（OpenAI 兼容，`{env:UAIG_TOKEN}`）与 MCP `uaig`；按 `opencode --version` 选择 V1 / V2 配置结构，也可以用 `--opencode-schema` 指定；模型写作 `uaig/<服务名>` |
| Pi | `~/.pi/agent/models.json`；技能 `.agents/skills/use-gateway/` | provider `uaig`（`api: openai-completions`，`apiKey: "$UAIG_TOKEN"`；`--pi-api anthropic-messages` 可选）；Pi 没有内置 MCP，技能会让它用 `gateway tools` / `gateway call` |

- 已有的 JSON 配置只改 `uaig` 这一个条目，其他内容和顺序不变；带注释的文件不改，只打印让你手工合并。改之前备份为 `.bak`，重复运行不会产生变化。
- 网关地址取环境变量 `UAIG_URL`（缺省 `http://127.0.0.1:8080`）；局域网里的另一台设备用管理页面「局域网共享」里的地址。
