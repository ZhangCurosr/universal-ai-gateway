# Universal AI Gateway 部署指南

本文面向部署网关的用户，是 [spec/12](../spec/12_deployment.md) 与 [spec/14 §4](../spec/14_ai_access.md) 的精简版。配置项的完整说明见 [spec/03](../spec/03_config.md)。

网关是单个二进制文件，数据存放在本地 SQLite 中，**本身不提供 TLS**。需要 HTTPS 时，在前面放一个反向代理（Caddy 或 nginx）。

## 1. 快速开始

```bash
go build -o bin/gateway ./cmd/gateway        # 纯 Go，无需 C 编译器
cp gateway.example.yaml gateway.yaml          # 按需修改，见下文
./bin/gateway check-config --config gateway.yaml
./bin/gateway serve --config gateway.yaml
```

或者使用发布工具一次性为全平台构建二进制与生成校验和（spec/12 §7）：

```bash
go run ./tools/release
```

产物输出在 `dist/` 目录（`universal-ai-gateway_<版本>_<GOOS>_<GOARCH>[.exe]`）。校验方法：
- **Linux / macOS**：
  ```bash
  cd dist && sha256sum -c SHA256SUMS
  ```
- **Windows (PowerShell)**：
  ```powershell
  Get-FileHash dist\universal-ai-gateway_*.exe -Algorithm SHA256
  ```

**首次启动**时，stderr 会打印一行：

```
BOOTSTRAP ADMIN TOKEN (shown once): uai_live_...
```

这个管理员 Token **只显示这一次**，请立即保存。之后用它调用管理 API（只监听本机 `127.0.0.1:8081`）：

```bash
ADMIN=uai_live_...   # 上面的管理员 Token

# 添加上游 Key（pool 必须是 gateway.yaml 中已配置的池）
curl -s -X POST http://127.0.0.1:8081/admin/api/keys \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"pool":"anthropic-main","name":"key-1","secret":"sk-ant-..."}'

# 为每个使用者 / 每个 AI 客户端创建一个客户端 Token，设置额度、RPM 与可用服务
curl -s -X POST http://127.0.0.1:8081/admin/api/clients \
  -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -d '{"name":"alice","role":"user","quota_nano_usd":10000000000,"rpm_limit":60,"allowed_models":["claude-sonnet-4-5"]}'
# 响应中的 "token" 就是该客户端的 Token，同样只返回这一次
```

`quota_nano_usd` 的单位是 10⁻⁹ 美元：上例 `10000000000` = 10 美元。

**务必离线备份 `master_key_file`**（默认 `./secrets/master.key`）。它不能位于数据目录下；丢失主密钥意味着所有上游 Key 都无法再解密。

## 2. 选择部署形态

| 形态 | `data_listen` | `admin_listen` | TLS | `trusted_proxies` |
|:--|:--|:--|:--|:--|
| A. 仅本机 | `127.0.0.1:8080` | `127.0.0.1:8081` | 不需要 | `[]` |
| B. 局域网 | `0.0.0.0:8080` | `127.0.0.1:8081` | 可选 | `[]` |
| C. 域名对外 | `127.0.0.1:8080` | `127.0.0.1:8081` | 由反向代理负责 | 反向代理的地址，如 `["127.0.0.1/32", "::1/128"]` |

三条硬性规则：

- **形态 C 的数据面只能监听回环或内网地址。** 否则别人可以绕过反向代理直连网关，并伪造 `X-Forwarded-For`。
- **`trusted_proxies` 只填反向代理自身的地址。** 不要填 `0.0.0.0/0` 或整段公网地址。
- **管理面在任何形态下都只监听本机。** 需要远程管理时，用 SSH 隧道：`ssh -L 8081:127.0.0.1:8081 user@gateway-host`。

### 2.1 局域网（形态 B）

> 最简单的做法：打开管理页面的「局域网共享」，那里显示是否已开放、本机的局域网地址、可以复制的防火墙命令和另一台设备的配置片段，并可以一键开关（会写回 `gateway.yaml` 的 `server.data_listen`，原文件备份为 `.bak`，数据面短暂重启）。下面是手工做法。

1. 在 `gateway.yaml` 中设置 `server.data_listen: "0.0.0.0:8080"`。
2. 在防火墙中放行入站 TCP 8080：
   - Windows（只放行「专用网络」）：
     ```powershell
     New-NetFirewallRule -DisplayName "AI Gateway" -Direction Inbound -Protocol TCP -LocalPort 8080 -Action Allow -Profile Private
     ```
   - Linux（ufw）：
     ```bash
     sudo ufw allow from 192.168.0.0/16 to any port 8080 proto tcp
     ```
3. 局域网客户端把地址设为 `http://<网关IP>:8080`（见第 5 节）。

> 局域网内是明文 HTTP，同网段的设备可以嗅探到 Token。在公共或不可信网络中，请改用形态 C（内网域名 + 自签证书也可以）。

## 3. 域名对外（形态 C）

gateway.yaml 需要设置：

```yaml
server:
  data_listen: "127.0.0.1:8080"
  trusted_proxies: ["127.0.0.1/32", "::1/128"]
```

然后任选一种反向代理：

- **Caddy（推荐，自动申请和续期证书）：** 见 [deploy/Caddyfile.example](../deploy/Caddyfile.example)。
- **nginx：** 见 [deploy/nginx.conf.example](../deploy/nginx.conf.example)。必须设置 `proxy_buffering off`，否则流式输出会被攒成一整块再发。
- **Cloudflare Tunnel、frp 等内网穿透：** 把隧道客户端所在的地址（通常是 `127.0.0.1/32`）加入 `trusted_proxies`。部分隧道会限制单个请求的时长，部署前请用一个长输出的请求实测。

反向代理的几个要点：

| 场景 | 要求 |
|:--|:--|
| 请求体上限 | 代理的上限要和 `limits.max_request_body_bytes` 一致（默认 32 MiB）。Caddy 写 `32MiB`（`MB` 是十进制），nginx 写 `32m` |
| 流式输出（SSE） | Caddy 设 `flush_interval -1`；nginx 设 `proxy_buffering off` |
| `/mcp` | 原样转发 `Authorization`、`MCP-Protocol-Version`、`Origin` 头（两个示例默认都会转发）。浏览器类 MCP 客户端不受支持 |
| 文件上传（流式请求体的路由） | 只对这些路径把上限放宽到 `limits.max_stream_body_bytes`（默认 512 MiB）；nginx 还要关闭 `proxy_request_buffering`。网关的 `server.read_timeout` 按「文件大小 ÷ 最低上行带宽」调大。MinerU 的本地文件上传端点是 `/v1/parse/file`、`/v1/tasks/file`（单文件上限 200 MiB）；`/v1/parse`、`/v1/tasks` 只接受带文件 URL 的 JSON（≤ 64 KiB），不需要放宽 |
| 同步封装的长任务 | 代理的读超时必须 ≥ 该池的 `timeouts.response_header_base`，否则代理先断开，而上游照样收费 |

### 3.1 对外前检查清单

- [ ] `data_listen` 是 `127.0.0.1:8080`，从公网直连 8080 不通。
- [ ] `trusted_proxies` 只含反向代理的地址。从外部发一个带伪造 `X-Forwarded-For` 的请求，访问日志里的 `client_ip` 仍是真实来源。
- [ ] 反向代理的请求体上限与 `max_request_body_bytes` 一致。
- [ ] 流式请求能逐字输出，而不是结束时一次性出现。
- [ ] `/mcp` 可用：`UAIG_URL=https://域名 UAIG_TOKEN=... gateway tools` 能列出工具；不带 Token 时返回 401。
- [ ] 如果接入了上传类 API，用接近上限的大文件实测一次，确认代理和 `read_timeout` 不会中途断开。
- [ ] `limits.preauth_ip_rpm` 已启用（默认 600）。
- [ ] 每个客户端都设置了额度和 `rpm_limit`；普通客户端没有 `quota_unlimited`。
- [ ] `master_key_file` 不在数据目录下，并且已离线备份。

## 4. 资源与调参

| 机器内存 | `max_in_flight_requests` | `max_buffered_body_bytes` | `max_request_body_bytes` |
|:--|:-:|:-:|:-:|
| 1 GiB | 64 | 128 MiB | 16 MiB |
| 2 GiB | 128 | 256 MiB | 32 MiB |
| ≥ 4 GiB | 256（默认） | 512 MiB（默认） | 32 MiB（默认） |

请求体相关内存约为 `3 × max_buffered_body_bytes`。缓冲响应每份最多 64 MiB，并另外共享一份 `max_buffered_body_bytes` 预算（默认 512 MiB），读取时预算不足返回 503 `overloaded`。请求体和响应的预算分别计数；响应关闭连接后，预算仍保留到当前调用尝试结束，避免转译与转发期间重复使用。流式传输不按累计传输量消耗这份预算。JSON 解析、切片扩容、转译和 MCP 结果还会占用额外内存，因此这些预算不是进程堆的硬上限。调小 `max_request_body_bytes` 时，反向代理的上限要一起调小。

客户端每次写入的超时复用池的 `timeouts.stream_idle`，每次写入都会续期；长流持续正常收发不会因固定总时长被截断，停止读取的客户端会超时退出并释放资源。

上游 HTTP 重定向不会自动跟随，包括同域重定向及 HTTPS 到 HTTP 的降级。池的 `base_url` 应指向最终 API 地址；依赖跳转到另一地址的下载接口会返回错误，不能通过重定向把网关持有的凭据传到目标站点。

**实测基准参考（T50 基线报告）**：
- **默认档（≥ 4 GiB，256 路长流式并发）**：Heap 峰值约 **84 MiB**，系统内存 Sys 峰值约 **117 MiB**，首包延迟 TTFB 约 0.57 ms。
- **最小档（1 GiB，64 路长流式并发）**：Heap 峰值约 **97 MiB**，系统内存 Sys 峰值约 **121 MiB**。
- 流式请求由于采用边收边发的 chunk 级处理管道，日常流式并发占用远低于理论缓冲积压上限，极度节省资源。详细报告见 [docs/handoff/reports/T50_perf_baseline.md](handoff/reports/T50_perf_baseline.md)。

## 5. 开机自启与崩溃自动重启

网关是单进程，进程一退出服务就中断，所以要交给系统的服务管理器自动拉起。

**计费持久化边界**：强制杀进程会丢失所有尚未持久化的计费事件，包括队列、批次和重试缓冲。`ledger.flush_interval` 只是正常运行时的刷盘触发间隔，数据库故障与重试会令事件跨多个周期积压，不能把它当作崩溃丢失窗口的上界。正常关闭会尝试最终刷盘，失败时写入 `data_dir/unflushed_usage.jsonl` 并在下次启动恢复；这不等同于强杀时有持久化日志保证。

多步骤付费任务在提交成功后，即使后续轮询、结果下载失败或客户端取消，也会记录已产生的费用；页数已知按实际上游用量，未知按预留估算。失败不会触发再次提交任务。

**Linux（systemd）**：使用 [deploy/systemd/universal-ai-gateway.service](../deploy/systemd/universal-ai-gateway.service)，文件开头的注释里有安装步骤。`TimeoutStopSec=40s` 比 `shutdown_grace`（默认 30s）大，给关闭时排空在途请求和计费刷盘留出时间。如果调大了 `shutdown_grace`，这个值要一起调大。首次启动后，用 `journalctl -u universal-ai-gateway` 查看引导 Token。

**Windows（任务计划程序）**：

1. 先在前台运行一次 `C:\uaig\bin\gateway.exe serve --config C:\uaig\gateway.yaml`，记下引导 Token，然后按 Ctrl+C 退出。
2. 以管理员身份打开 PowerShell，执行：
   ```powershell
   .\deploy\windows\install-task.ps1 -InstallDir C:\uaig
   ```
   脚本会先校验配置，再注册一个开机时以 SYSTEM 身份运行、退出后每分钟重启的计划任务。
3. 以 SYSTEM 身份运行时，`data_dir` 和 `master_key_file` 所在的目录必须对 SYSTEM 可写。

### 5.1 修改配置后生效（热重载）

改完 `gateway.yaml` 后不需要重启进程，运行：

```bash
export UAIG_ADMIN_URL=http://127.0.0.1:8081   # 缺省就是这个地址
export UAIG_ADMIN_TOKEN=<管理员 Token>
gateway reload --dry-run   # 先看会改什么，不应用
gateway reload             # 应用
```

也可以直接调用管理 API：`POST /admin/api/reload`（请求体可选 `{"dry_run": true}`），`GET /admin/api/reload` 查看最近 20 次重载记录。

网关比较新旧配置，自动选择需要的方式：

| 方式 | 哪些字段 | 对客户端的影响 |
|:--|:--|:--|
| **热重载** | `pools`、`models`（含价格、接口声明）、`mcp`、`translation`、`server.log_level`、`server.trusted_proxies`、`server.admin_hosts` | 没有影响：进行中的请求按旧配置完成（包括计费价格），新请求立即使用新配置；Key 的冷却、熔断、亲和状态保留 |
| **进程内重启** | 其他全部字段：监听地址、`data_dir`、`master_key_file`、超时、`limits`、`ledger`、`audit` | 进行中的请求最多等 `shutdown_grace` 完成；监听地址不变时，重启期间的新连接排队等待、不会被拒绝；计费事件全部落库后才重启。Key 的冷却与熔断状态会重置（与重启进程相同） |

- **先检查、再动手**：配置有错（400）、无法构造（422，例如引用了不存在的协议）、新监听地址已被占用、新数据目录不可用，都会直接报错，**旧配置继续生效**。
- **重启失败会回滚**：进程内重启后新配置启动失败（例如新数据目录中的数据库损坏），网关用旧配置重新启动，并在重载记录中标记 `rolled_back`。只有回滚也失败时进程才退出（交给第 5 节的系统服务自动拉起）。
- **删除池**：池里的 Key 保留在数据库中，不再被使用，Key 列表显示为 `orphaned`；以后再加回同名的池，这些 Key 自动恢复使用。
- **换数据目录**：等于换一个新数据库（客户端、Key、用量都从空开始），新实例会生成新的引导管理员 Token 并打印到 stderr，原来的管理员 Token 在新库里不存在。`--dry-run` 会提示这一点。
- **主密钥文件**：只有新文件中的密钥与当前相同（例如只是移动了文件），或同时更换数据目录时才允许修改；否则拒绝，因为已保存的 Key 与 Token 都依赖当前主密钥。
- 不会自动监视配置文件：修改后必须显式执行 `gateway reload`（避免读到只保存了一半的文件）。
- 重载记录与日志中只有池名、服务名和字段名，不包含配置内容；配置错误的原文只出现在命令输出和 `GET /admin/api/reload` 中。

## 6. 客户端接入

> 让其他项目里的 AI 助手使用网关：把 [AI_QUICKSTART.md](./AI_QUICKSTART.md) 交给它，或者安装技能 [`docs/skills/use-gateway`](./skills/use-gateway/SKILL.md)（复制到 `~/.claude/skills/use-gateway/`，所有项目都能用）。

先用管理 API 为每个 AI 客户端创建 Token，并用 `allowed_models` 限制它能用的服务。下文中 `https://gw.example.com` 换成你的地址：局域网形态用 `http://<网关IP>:8080`。

### 6.1 作为模型后端

| 客户端 | 配置方式 |
|:--|:--|
| **Claude Code** | `export ANTHROPIC_BASE_URL=https://gw.example.com`（注意**不带** `/v1`）<br>`export ANTHROPIC_AUTH_TOKEN=uai_live_...` |
| **Codex CLI** | `~/.codex/config.toml` 中配置 provider，`wire_api = "responses"`（使用 `/v1/responses`） |
| **OpenAI SDK** | `base_url="https://gw.example.com/v1"`，`api_key="uai_live_..."` |
| **Cursor** | 设置中覆盖 OpenAI Base URL 为 `https://gw.example.com/v1`，API Key 填客户端 Token |

#### 详细配置示例

**Claude Code**：
```bash
# 环境变量注入（注意基地址不带 /v1）
export ANTHROPIC_BASE_URL=https://gw.example.com
export ANTHROPIC_AUTH_TOKEN=uai_live_...
```

**Codex CLI**（编辑 `~/.codex/config.toml`）：
```toml
[providers.uaig]
base_url = "https://gw.example.com/v1"
env_key = "UAIG_TOKEN"
wire_api = "responses"
```
```bash
export UAIG_TOKEN=uai_live_...
codex --provider uaig
```

**OpenAI SDK（Python）**：
```python
from openai import OpenAI

client = OpenAI(
    base_url="https://gw.example.com/v1",
    api_key="uai_live_...",
)

response = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "你好"}],
)
print(response.choices[0].message.content)
```

**Cursor IDE**：
1. 打开 Cursor 设置（`Ctrl+,` 或 `Cmd+,`）-> **Models**。
2. 开启 **OpenAI API Key**。
3. 展开 **Override OpenAI Base URL**，填写：`https://gw.example.com/v1`。
4. 在 API Key 输入框中填入网关发放的客户端 Token：`uai_live_...`。
5. 在模型列表中点击 **Add Model** 添加网关配置的服务名称（如 `deepseek-chat` 或 `gpt-5`）。

**Gemini 客户端（Google GenAI SDK 与 Gemini CLI）**：
网关支持 Google AI Studio 的 Gemini API（`generateContent`、`streamGenerateContent`、`countTokens` 以及模型列表，spec/08 §10 / T47）。
- **Python（google-genai SDK）**：
  ```python
  from google import genai
  from google.genai import types

  # 指向网关数据面（不要在末尾带 /v1beta，SDK 会自动补齐端点路径）
  client = genai.Client(
      api_key="uai_live_...",
      http_options=types.HttpOptions(base_url="https://gw.example.com"),
  )

  # 非流式生成
  response = client.models.generate_content(
      model="gemini-2.5-pro",
      contents="用一句话解释微内核架构",
  )
  print(response.text)

  # 流式生成（SDK 自动请求 alt=sse）
  for chunk in client.models.generate_content_stream(
      model="gemini-2.5-pro",
      contents="数到五",
  ):
      print(chunk.text, end="")
  ```
- **Gemini CLI**：
  ```bash
  export GEMINI_API_KEY="uai_live_..."
  # 将官方 API 基础地址指向网关
  export GOOGLE_GENAI_BASE_URL="https://gw.example.com"
  gemini "你好"
  ```
- **安全与凭据说明**：网关**不支持**在 URL 查询串中传 `?key=<Token>`（AUTH-03，避免凭据在反向代理、访问日志和监控中泄露）。客户端必须将网关 Token 放在请求头 `x-goog-api-key` 或 `Authorization: Bearer` 中传递。
- **阶梯定价配置提示**：Gemini 官方部分模型（如 Gemini 2.5 Pro）根据单次请求 Prompt 是否超过 200k 实行不同阶梯单价。当前网关的静态价格表按单一单价计费，建议在 `gateway.yaml` 的 `models[].price` 中按你的常见使用场景（例如 200k 以内的主流档位）配置基准单价。

> **提示（智能体客户端字段容忍）**：接入 Claude Code、Codex CLI 等智能体客户端进行跨协议转译时（例如客户端使用 Anthropic/Responses 协议，目标上游为 OpenAI Chat 等），智能体客户端常会随版本升级附带实验性或环境专用的未定义字段。建议在 `gateway.yaml` 中配置 `translation.unknown_fields: drop`（spec/10 §1 第 5 条），容忍并丢弃未知字段，避免请求被默认策略严格 400 阻断。

### 6.2 作为 MCP 工具

网关提供 `/mcp` 端点（spec/14），让 AI 客户端能够把网关本身当作 MCP 工具服务端调用。

**远程方式（HTTP 传输，推荐）**：客户端机器无需安装网关二进制：
```bash
claude mcp add --transport http uaig https://gw.example.com/mcp --header "Authorization: Bearer uai_live_..."
```

**本地 stdio 桥方式（跨进程 stdio）**：用于只支持 stdio 传输的客户端：
在 `claude_desktop_config.json` 或项目 `.mcp.json` 中配置：
```json
{
  "mcpServers": {
    "uaig": {
      "command": "gateway",
      "args": ["mcp"],
      "env": {
        "UAIG_URL": "https://gw.example.com",
        "UAIG_TOKEN": "uai_live_..."
      }
    }
  }
}
```

#### 工具目录：工具多了也不占上下文（CAT-09）

MCP 客户端一连上就会读取工具列表，把每个工具的说明和参数 Schema 都放进 AI 的上下文，一个工具大约 150–300 token。接入的接口越多，这部分开销越大，AI 选错工具的概率也越高。`mcp.tool_mode` 控制工具怎么列出：

| `tool_mode` | 行为 |
|:--|:--|
| `auto`（缺省） | 该客户端能看到的工具不超过 `auto_threshold`（缺省 8）个时直接列出，否则切换为目录 |
| `direct` | 总是直接列出全部工具 |
| `catalog` | 总是只列出目录工具 |

目录模式下 `tools/list` 只有 3 个目录工具（加上 `pinned_tools` 中的常用工具），常驻上下文约 600 token，与接口数量无关。AI 按三步使用：

```
gateway_search_tools  {"query": "判断"}            → 每个工具一行：名称 — 说明
gateway_describe_tool {"name": "bivia_boolean"}    → 完整的参数 Schema、是否只读
gateway_call_tool     {"name": "bivia_boolean", "arguments": {...}}   → 调用结果（照常计费）
```

```yaml
mcp:
  tool_mode: auto
  auto_threshold: 8
  pinned_tools: [tavily_search]   # 目录模式下仍然直接列出
```

- 搜索、查看只针对该客户端白名单内的工具，不访问上游、不计费；其他客户端的工具搜不到，按名字查看时与「不存在」得到同样的提示。
- 目录模式下工具只是不列出，仍可用原名直接调用：`gateway call <工具名>` 和已有的客户端配置照常可用；`gateway tools` 会翻页列出全部真实工具。
- 代价是 AI 调用不常用的工具要多走一两步；最常用的一两个工具放进 `pinned_tools`。

#### 让 AI 管理网关：管理面 MCP

除了使用服务，AI 也可以管理网关本身（查看状态与用量、启用或禁用 Key、调整客户端额度、重载配置）。管理面 MCP 只在管理面（本机回环地址）提供，需要**管理员 Token**：

```bash
# 远程方式（与网关在同一台机器，或经 SSH 端口转发）
claude mcp add --transport http uaig-admin http://127.0.0.1:8081/admin/mcp --header "Authorization: Bearer <管理员 Token>"
```

或者用 stdio 桥接（`UAIG_ADMIN_URL` 缺省 `http://127.0.0.1:8081`）：

```json
{ "mcpServers": { "uaig-admin": { "command": "gateway", "args": ["admin-mcp"],
  "env": { "UAIG_ADMIN_URL": "http://127.0.0.1:8081", "UAIG_ADMIN_TOKEN": "<管理员 Token>" } } } }
```

| 工具 | 作用 | 只读 |
|:--|:--|:--|
| `gateway_admin_status` / `_list_clients` / `_list_keys` / `_usage` / `_config` / `_tools` / `_reload_records` | 查看状态、客户端、Key（只有掩码）、用量、配置、AI 工具、重载记录 | 是 |
| `gateway_admin_update_client` | 修改客户端的额度、速率、允许的服务、禁用 | 否 |
| `gateway_admin_set_key_state` | 启用或停用一把上游 Key | 否 |
| `gateway_admin_reload` | 重载配置文件（建议先 `dry_run`） | 否 |

**命令行**（任何能执行 shell 命令的 AI CLI 都能用，包括没有 MCP 的 Pi；连接信息同上）：

```bash
gateway admin tools                                   # 列出工具：短名、read / write、说明
gateway admin call pools                              # 池的可用数、额度、预计用完时间（JSON）
gateway admin call key_stats --args '{"pool":"tavily"}'
gateway admin call set_key_state --args '{"id":12,"state":"disabled"}' --yes   # 写入类必须加 --yes
```

**有意不提供**新建客户端、轮换 Token、添加 Key：这些操作会把凭据交给 AI。新建客户端请用管理页面；添加 Key 用下面的 `gateway keys import`，密钥在本机读取，不经过 AI。

> 管理员 Token 有完整的管理权限。只把管理面 MCP 配给你信任的本机 AI 客户端，不要把管理面暴露到局域网或公网。

### 6.3 作为命令行（CLI）

网关内置命令行子命令（`gateway tools` 与 `gateway call`），方便脚本与运维调用。连接信息必须通过环境变量传递（CLI-05），不得使用命令行参数传递 Token：

```bash
export UAIG_URL=https://gw.example.com
export UAIG_TOKEN=uai_live_...

# 列出网关支持的所有工具（名称 + 单行描述）：
gateway tools

# 列出完整 JSON 格式的工具清单（包含参数 JSON Schema）：
gateway tools --json

# 直接通过 JSON 字符串调用工具：
gateway call ask_model --args '{"model":"deepseek-chat","input":"用一句话解释 CAP 定理"}'

# 从标准输入（stdin）管道读入参数（--args -）：
cat query.json | gateway call ask_model --args -

# 退出码约定：
#   0: 成功且 isError 为 false
#   1: 执行失败或上游返回错误（isError 为 true）
#   2: 参数格式错误（非合法的 JSON 对象）
```

> **安全与计费提示**：工具调用与模型调用同样会计费并扣减额度。给 AI 客户端分配的 Token 必须设置总额度（`quota_nano_usd`）与 RPM 上限（`rpm_limit`），并通过 `allowed_models` 严格限制可调用的模型与工具范围。

## 7. 监控与告警（Prometheus）

网关在管理面监听上暴露 Prometheus 纯文本格式指标端点 `GET /admin/metrics`（无需鉴权，依赖管理面仅监听本地回环地址）。

### 7.1 Prometheus 抓取配置

```yaml
scrape_configs:
  - job_name: 'universal-ai-gateway'
    metrics_path: '/admin/metrics'
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:8081']
```

### 7.2 核心告警规则示例

```yaml
groups:
  - name: uaig_alerts
    rules:
      # 计费事件积压：队列已使用超过 80%
      - alert: UAIGLedgerQueueNearFull
        expr: uaig_ledger_events_queue_length / uaig_ledger_events_queue_capacity > 0.8
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Universal AI Gateway 计费事件队列接近占满"
          description: "计费事件积压已超过 80%，可能发生磁盘写入瓶颈。"

      # 5xx 错误率升高
      - alert: UAIGHighErrorRate
        expr: sum(rate(uaig_requests_total{code="5xx"}[5m])) / sum(rate(uaig_requests_total[5m])) > 0.05
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Universal AI Gateway 5xx 请求错误率 > 5%"

      # 某个上游 Key 池全线熔断
      - alert: UAIGUpstreamPoolAllBreakersOpen
        expr: sum by (pool) (uaig_keys_status{state="breaker_open"}) == count by (pool) (uaig_key_inflight)
        for: 30s
        labels:
          severity: critical
        annotations:
          summary: "上游 Key 池 {{ $labels.pool }} 内所有 Key 均已熔断"

      # 审计记录发生丢弃
      - alert: UAIGAuditRecordsDropped
        expr: rate(uaig_audit_dropped_total[5m]) > 0
        for: 1m
        labels:
          severity: warning
        annotations:
          summary: "审计流水线队列满，记录开始发生丢弃"
```

> 管理面会检查 `Host` 头（spec/09 UI-20）：Prometheus 用 `127.0.0.1:8081`、`localhost:8081` 抓取不受影响；如果用别的主机名指向管理面，要把它加进 `server.admin_hosts`，否则会得到 `421`。

## 8. 管理页面（Web UI）

网关内置一个管理页面，覆盖管理 API 的全部操作：概览、客户端（新建、编辑、禁用、轮换 Token）、上游 Key（新增、编辑、停用、恢复）、用量查询。页面打包在网关二进制里，不需要另外部署，也不从外部网络加载任何资源。

### 8.1 打开页面

网关启动后，日志里会有一行 `admin ui`，给出地址，默认是：

```
http://127.0.0.1:8081/admin/ui/
```

用管理员 Token 登录。第一次使用时，就是首次启动时 stderr 打印的那个 `BOOTSTRAP ADMIN TOKEN`（见 §1）。

- Token 只保存在当前浏览器标签页（`sessionStorage`）里，关闭标签页就要重新输入。页面不使用 Cookie。
- 新建客户端、轮换 Token 后，新 Token **只显示一次**，请当场复制保存。
- 界面有中文和英文，按浏览器语言自动选择，左下角可以切换。

「配置」页（`#/config`）：修改 `gateway.yaml` 后，在这里先「预览重载」再「应用重载」（§5.1），并查看最近的重载记录、各服务与 AI 工具（含接口声明）。上游 Key 页中，池已从配置中删除的 Key 显示为「孤立」。

### 8.2 从另一台电脑访问

管理面只监听本机回环地址（CFG-02），**不要**用反向代理把它暴露到局域网或公网。远程管理时用 SSH 端口转发：

```bash
ssh -L 8081:127.0.0.1:8081 user@gateway-host
```

然后在自己电脑的浏览器里打开 `http://127.0.0.1:8081/admin/ui/`。

### 8.3 Host 检查

为了防止恶意网页借你的浏览器访问本机管理面（DNS 重绑定攻击），管理面只接受 `Host` 为 `127.0.0.1`、`::1`、`localhost` 的请求，其他一律返回 `421`。确实需要用别的主机名访问时（例如 hosts 文件里给本机起的名字），把它加进配置：

```yaml
server:
  admin_hosts: ["gateway.local"]
```

### 8.4 忘记管理员 Token

页面和 API 都不能找回 Token（数据库里只存哈希）。如果还有别的管理员，用它登录后给丢失 Token 的管理员轮换一个新的。网关目前没有内置的「最后一个管理员丢失 Token」恢复命令（引导 Token 只在数据库里一个管理员都没有时才生成），所以**建议至少保留两个管理员客户端**，各自妥善保存 Token。

## 9. 接入任意 REST API（通用转发适配器）

> **安全警告（RS-30）**：
> 网关的通用 REST 转发不对路径和方法设白名单。**上游 Key 的全部权限都暴露给被允许访问该服务的客户端**。
> 1. 请务必给 `rest` 池配置**最小权限的 Key**（如只读 Token、限定细粒度作用域的 Token）。
> 2. 当需要读写分离时，对同一个上游配置**两个独立的池与两个服务名**（例如只读服务 `github-ro` 配置只读 Key、读写服务 `github-rw` 配置读写 Key），再通过客户端的 `allowed_models` 严格控制谁能调用哪一个。

通用 REST 适配器（`protocol: rest`，spec/08 §8.3）用于接入无需解析请求体的任意非大模型 API（换上游 Key、按次计费、透明转发），无需编写代码。

### 9.1 配置示例

在 `gateway.yaml` 中增加池与模型定义：

```yaml
pools:
  - name: weather-pool
    protocol: rest
    base_url: "https://api.weather.example/v2"

models:
  - name: weather               # 客户端以 /rest/weather/... 访问
    pool: weather-pool
    price: { requests: "0.001" } # 按次计费：每次成功请求 0.001 美元
    reserve: { requests: 1 }    # 预留 1 次
```

客户端调用示例：
```bash
curl -H "Authorization: Bearer <网关客户端Token>" \
  "https://gw.example.com/rest/weather/forecast?city=Paris"
# 网关自动换上池中的上游 Key，透明转发至：
# GET https://api.weather.example/v2/forecast?city=Paris
```

### 9.2 上游 Key 放置方式

通过管理 API 添加上游 Key 时，把 Key 的放置方式编码在 `secret` 字符串中（解密后生效，管理面不校验格式）：

| 写法 | 上游请求行为 | 适用场景 |
|:--|:--|:--|
| `bearer:<token>` | `Authorization: Bearer <token>` | 大多数现代 REST API（默认方式） |
| `header:<Name>:<val>` | 添加自定义请求头 `<Name>: <val>` | 例如 `header:X-Api-Key:my-key-val` |
| `query:<name>:<val>` | 在 URL 查询串中注入 `<name>=<val>` | 例如 `query:api_key:my-key-val`（客户端同名参数会被自动剔除并替换） |
| `basic:<user>:<pass>` | `Authorization: Basic base64(user:pass)` | HTTP Basic 认证，密码中可含冒号 |
| 直接写 token（无前缀） | 自动视为 `bearer:<token>` | 兼容常见 Bearer 认证 |

格式非法（如 `header:` 缺少冒号或头名非法）时，网关将返回认证失败并自动隔离该 Key（RS-20）。

#### 批量导入 Key（密钥不经过 AI）

**在管理页面导入**：「上游 Key」页右上角的「批量导入」。粘贴内容或选择本地文件（每行一把 Key、JSONL、CSV，最大 8 MiB），选好池与字段后先看预览的数量，再点「导入」。文件只在浏览器里解析，只有密钥、过期时间和标签会发给网关；页面上不显示任何密钥。选项与下面的命令一一对应（字段过滤对应 `--where`，密钥前缀对应 `--secret-prefix`，「更新已有 Key」对应 `--update`），名称编号接在池中已有的最大编号之后。

**用命令行导入**：有一批账号文件时，用 `gateway keys import` 在本机读取文件并逐个添加。命令的任何输出都不包含密钥或文件中的其他字段（邮箱、密码），所以可以放心地让 AI 代为运行：

```bash
export UAIG_ADMIN_TOKEN=<管理员 Token>
# JSONL：每行一个 JSON 对象，取 api_key 字段，只导入 status 为 ok 的行；先 --dry-run 看计数
gateway keys import --pool bivia-pool --file accounts.jsonl --field api_key --where status=ok   --secret-prefix bearer: --name-prefix bivia --dry-run
gateway keys import --pool bivia-pool --file accounts.jsonl --field api_key --where status=ok   --secret-prefix bearer: --name-prefix bivia
# 输出：imported 84, duplicate 0, skipped 2 (filtered or invalid), failed 0, updated 0, expired 0

# 纯文本：每行一个密钥，忽略空行与 # 注释
gateway keys import --pool weather-pool --file keys.txt --format lines --secret-prefix "header:X-Api-Key:"
```

- Key 的名称是 `--name-prefix` 加三位序号（`bivia-001`、`bivia-002`……），不取自文件（文件里常有个人信息）。
- 已经存在的 Key（同一密钥）计为 `duplicate`，可以放心重复运行。
- 被 `--where` 过滤、缺少字段或不是 JSON 的行计为 `skipped`，stderr 中只写行号与原因。

##### 过期时间与标签

账号有过期时间时，让导入命令直接读取，过期的 Key 不会再被调度（管理页面显示「已过期」），也不需要先用脚本过滤文件：

```bash
# 读取 expires_at 字段；不带时区的时间按北京时间解释；所有 Key 打上来源标签
gateway keys import --pool mineru --file mineru_accounts.jsonl --expires-field expires_at --tz +08:00 --label signup-2026-11
# 账号续期后重新运行，加 --update 更新已存在 Key 的过期时间与标签
gateway keys import --pool mineru --file mineru_accounts.jsonl --expires-field expires_at --tz +08:00 --label signup-2026-11 --update
```

- 时间格式：RFC 3339、Unix 秒或毫秒、`2026-12-01 16:30:00`、`2026-12-01`（当天 0 点）。字段为空表示不过期；无法识别的时间跳过该行。
- 文件中已经过期的账号默认跳过，计入 `expired`；确实要导入时加 `--include-expired`。
- `--label-field batch` 用文件中的字段作为标签；管理页面可以按标签筛选。
- 过期只在内存中判断，不改库中状态：在管理页面或用 `--update` 把过期时间改到将来，Key 立即恢复。

##### 让池跟着账号文件走：`gateway keys sync`

注册程序不断往账号文件里追加新账号、把失效的账号标成 `banned` 时，用 `sync` 代替 `import`（T64）：

```bash
# 一次性同步：新增、更新过期时间与标签，停用文件中已经没有或 status 不再是 ok 的账号
gateway keys sync --pool tavily --file tavily_accounts.jsonl --where status=ok --label signup-tavily

# CSV 账号文件可以直接读（第一行是表头）；--watch 让它一直运行，文件有变化就同步
gateway keys sync --pool mineru --file mineru_accounts.csv --format csv --expires-field expires_at --tz +08:00 \
  --label signup-mineru --watch 10m
```

- 只停用**同一池中、标签等于 `--label`** 的 Key；手工添加的 Key、管理员停用的 Key、因 401 隔离的 Key 都不会被动到。
- 同步停用的 Key 原因显示为 `sync: not in source`；账号回到文件中后自动恢复。
- 一次要停用超过一半时拒绝执行（文件可能写坏了），确认无误后加 `--force`。
- 名称编号接在池中已有的最大编号之后，可以放心重复运行。`import` 也支持 `--format csv`。
- Windows 上可以用「任务计划程序」每隔一段时间运行一次（不加 `--watch`），或者在一个终端里加 `--watch` 常驻。

##### 池子快空时提醒：告警

在池上设置阈值，网关每分钟检查一次；有告警时管理页面概览最上面会显示，`gateway_admin_alerts`（管理面 MCP）可以让 AI 查询，也可以推送到群机器人（T64）：

```yaml
alerts:
  check_interval: 1m
  repeat: 24h                    # 持续告警时每 24 小时再推送一次
  webhook:
    url_file: ./secrets/alert_webhook.url   # 文件里只放一行机器人地址（地址常带令牌，不要写进配置）
    format: feishu               # generic | slack | feishu | wecom | dingtalk

pools:
  - name: tavily
    alerts:
      min_available_keys: 5      # 可用 Key 少于 5 把（一把都没有时为「严重」）
      min_quota_left: 500        # 全池本期剩余额度少于 500（需要 quota）
      expiring_within: 72h       # 有 Key 将在 72 小时内过期
```

- 告警开始、级别变化、恢复时各推送一次；推送失败只记日志，不影响网关。
- Prometheus 指标：`uaig_pool_available_keys`、`uaig_pool_quota_left`、`uaig_pool_keys_expiring`、`uaig_alerts_firing`。

##### 池还够用吗：Key 视图

管理页面「上游 Key」的每个池卡片标题下有一行摘要：可用 Key 数、本期剩余额度、最近 24 小时的消耗速度，以及「预计 {时间} 用完」或「能撑到 {时间} 重置」；表格里的「24 小时」列是每把 Key 的请求数与失败率，鼠标悬停可以看到各类失败（限流、鉴权、超时……）的次数（T65）。

- 失败率按每次**尝试**统计（包括被换 Key 重试掉的那一次），只在内存中，重启后从零开始；客户端自己的错误（4xx）不算 Key 的失败。
- 管理 API：`GET /admin/api/pools`、`GET /admin/api/keys/stats?pool=&from=&to=`（用量与费用可以选最长 31 天）。
- 管理面 MCP：`gateway_admin_pools`、`gateway_admin_key_stats`。接好管理面 MCP 后可以直接问 AI「tavily 池这个月还够用吗」。

##### 周期额度：用满前主动换 Key

账号有「每月 N 次」「每天 N 页」「试用共 N 点」这类额度时，在池上声明，网关会按 Key 统计本期用量，用满就不再选它，直到重置时刻；不会先让上游失败一次（T63）：

```yaml
pools:
  - name: tavily
    protocol: tavily
    base_url: "https://api.tavily.com"
    quota:
      unit: credits            # 计量单位，必须出现在该池服务的 price 中
      limit: 1000              # 每把 Key 每周期的上限
      period: month            # day | week | month | total（total = 不重置，用完为止）
      reset: { day: 1, time: "00:00", tz: "+08:00" }   # 每月 1 日 0 点（北京时间）；tz 缺省为 UTC
      strategy: drain          # drain：一把用完再换下一把（缺省）；spread：平均分摊
```

- `week` 用 `reset: { weekday: mon, time: "00:00" }`；`day` 只写 `time` 与 `tz`；`total` 不写 `reset`。
- 用量来自计费记录，重启后准确；在已经运行的池上新加额度，本周期已经用掉的部分也会算进去。
- 用满的 Key 在管理页面显示「额度用尽」与重置时间；全部用满时客户端收到 503，`Retry-After` 为最早的重置时刻。
- 异步任务的查询（如 MinerU 的任务状态）不受额度限制，仍落在创建任务的 Key 上。
- 上限是软上限：同时在途的请求可能让用量略超上限。网关以外也在用同一把 Key 时计数会偏少，上游的拒绝仍按 9.8 节的错误规则冷却。
- 上限按池统一设置；同一池中额度不同的账号请分到不同的池。

### 9.3 自定义请求头转发（`X-UAIG-Forward-*`）

网关只转发白名单标准头（`Content-Type`、`Accept`、`Accept-Language`、`If-Match`、`If-None-Match`、`If-Modified-Since`、`If-Unmodified-Since`、`Range`）。

上游要求的业务专用头（如版本头），客户端使用 `X-UAIG-Forward-<Name>: <value>` 传入，网关会去掉前缀以 `<Name>: <value>` 转发给上游：
```bash
curl -H "Authorization: Bearer <Token>" \
     -H "X-UAIG-Forward-X-Api-Version: 2024-05-01" \
     "https://gw.example.com/rest/weather/forecast"
# 上游收到请求头：X-Api-Version: 2024-05-01
```
*注：安全敏感头（如 `Host`、`Authorization`、`Cookie`、`X-Forwarded-*`、`X-UAIG-*`）会被网关丢弃并记一条 Warn。*

### 9.4 路径前缀与幂等重试（`/rest/` 与 `/rest-idem/`）

内核遇到 5xx、429、超时、网络错误时会自动换 Key 重试（spec/07 §6）。但对于有副作用的 `POST` / `PATCH`，重复请求可能造成重复下单或产生副作用。

因此，网关由客户端显式选择路径前缀（RS-02、RS-19）：

| 路径前缀 | 支持方法 | 重试策略 | 适用场景 |
|:--|:--|:--|:--|
| `/rest/{service}/{path...}` | GET, HEAD, DELETE, PUT, POST, PATCH | GET / HEAD / PUT / DELETE 允许换 Key 重试；**POST / PATCH 流式透传，严格只尝试一次**（CORE-07） | 默认前缀。所有产生副作用的创建、修改、支付、推送接口 |
| `/rest-idem/{service}/{path...}` | **仅限 POST, PATCH** | 缓冲请求体，**失败自动换 Key 重试**（同一请求体可能被上游多次收到） | **仅限无副作用的只读查询类 POST 接口** |

**什么样的接口可以使用 `/rest-idem/`？**
- 许多检索或复杂查询 API 虽然出于参数体积考虑采用 `POST` 方法（如 Bivia 的 `/boolean`、`/score`、Elasticsearch `_search`、向量检索），但本质是只读、幂等的查询操作。走 `/rest-idem/` 可以在遇到单把 Key 429 或故障时自动换 Key 重试，保障高可用。
- 绝不得把转账、扣费、发送通知等有副作用的接口指向 `/rest-idem/`。

### 9.5 上游 403 风险与处理办法（RS-31）

- **行为规则**：通用 REST API 常使用 403 表示「资源级无权访问」（如访问未授权的组织资源），并非 Key 本身失效。网关将 403 作为客户端错误原样转发给客户端，**不会自动隔离 Key**（RS-16）。
- **风险提示**：如果某些上游在 Key 配额耗尽或账户被封禁时也返回 403，网关不会自动隔离该 Key，每次轮询到它都会失败。
- **排查与处理**：每次上游返回 403 时，网关会记录一条限频 Warn 日志（格式形如 `rest upstream returned 403; key not quarantined key_mask=bear****1234`）。如果观察到某个 Key 掩码反复出现 403 告警，请在管理面停用该 Key：
  ```bash
  # 通过管理 API 禁用该 Key
  curl -s -X PATCH http://127.0.0.1:8081/admin/api/keys/<KEY_ID> \
    -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
    -d '{"state":"disabled"}'
  ```

### 9.6 把接口暴露给 AI（RS-45）

> **接口声明不是权限边界**：声明接口只是让 AI 方便地调用它。被允许访问该服务的客户端，仍然可以直接请求 `/rest/{service}/` 下的任意路径与方法。权限仍然靠最小权限的 Key 和服务名拆分来控制（见本节开头的安全警告）。

在服务下用 `tools` 声明接口，网关就把它们作为 MCP 工具（以及 `gateway call` 命令）提供给 AI；调用时照常鉴权、计费、选 Key、重试。修改后需要重启网关。

```yaml
models:
  - name: bivia
    pool: bivia-pool
    price: { requests: "0.001" }
    reserve: { requests: 1 }
    tools:
      - name: bivia_boolean
        description: "对一段情境回答是或否的问题，返回答案、概率和是否弃答。"
        method: POST
        path: /boolean
        idempotent: true        # 无副作用：走 /rest-idem/，失败可换 Key 重试
        read_only: true
        input_schema:
          type: object
          properties:
            state:    { type: string, description: "需要判断的情境" }
            question: { type: string, description: "是非问题" }
          required: [state, question]
          additionalProperties: false

  - name: github-ro
    pool: github-ro-pool
    price: { requests: "0" }
    tools:
      - name: github_issue_get
        description: "读取一个 GitHub issue 的标题、正文、状态和标签。"
        method: GET
        path: /repos/{owner}/{repo}/issues/{number}
        input_schema:
          type: object
          properties:
            owner:  { type: string }
            repo:   { type: string }
            number: { type: integer }
          required: [owner, repo, number]
```

**参数怎么变成请求**：

| 参数 | 去向 |
|:--|:--|
| `path` 中 `{name}` 对应的参数 | 填进路径（单个路径段，`/` 会被转义为 `%2F`）；必须是字符串或数字，不能为空、`.`、`..`。在 `input_schema` 中必须声明并列入 `required` |
| `GET` / `DELETE` 的其余参数 | 查询串：标量一个值；标量数组重复同名参数；`null` 省略；对象被拒绝 |
| `POST` / `PUT` / `PATCH` 的其余参数 | 组成 JSON 请求体（数字原样保留） |

- `input_schema` 只给 AI 看，网关不做完整校验；参数是否合法由上游判断，上游的错误消息会作为工具错误返回给 AI。
- `idempotent: true` 只用于 `POST` / `PATCH`，表示接口没有副作用、可以重试；否则只尝试一次（与 9.4 相同）。
- `read_only` 缺省时 `GET` 为 true，其他方法为 false；多数 MCP 客户端会对非只读工具先征求确认。
- 响应：JSON 和文本原样返回给 AI（JSON 对象同时作为结构化结果）；非 2xx 返回 `HTTP <状态码>: ` 加错误体前 2000 字节；图片、PDF 等二进制响应不支持，返回工具错误。
- 只有 `rest` 池上的服务可以声明接口；工具名在整个网关内唯一，不能使用 `gateway_` 开头的三个目录工具名。

### 9.7 已知限制

1. **缺省按次计费**：没有 `api` 声明的服务每次成功请求计 1 个 `requests`。按响应字段、请求字段计费，以及业务错误码与异步任务，见 9.8。
2. **`/rest/` 下 POST / PATCH 不重试**：依据 CORE-07 规则，流式请求体严格只尝试一次。
3. **错误体截断**：上游返回的非 2xx 错误响应正文超过 8 KiB（`MaxErrorBodyBytes`）时会被截断。
4. **缺省无会话亲和**：异步任务的查询需要固定到创建它的 Key 时，用 9.8 的 `task_id` / `pinned_by`。
5. **无路径白名单**：网关不做请求路径与方法的内容过滤，安全控制依赖上游 Key 最小权限与模型访问列表（`allowed_models`）。接口声明（9.6）也不改变这一点。

### 9.8 声明业务错误、计费方式与异步任务（spec/08 §8.3.6）

很多 API 不只是「原样转发、按次计费」：额度用完时返回 HTTP 200 加业务错误码；按页数、积分计费；异步任务只能在创建它的账号下查询。在服务上加一个 `api` 块，通用转发就能处理这些情况，不需要专用适配器：

```yaml
pools:
  - name: mineru-rest
    protocol: rest
    base_url: "https://mineru.net"
    cooldown: { max: 24h }               # 日额度用完的 Key 要冷却到次日，不能被 10 分钟的缺省上限截断
models:
  - name: mineru-api
    pool: mineru-rest
    price: { pages: "0.01", requests: "0" }   # 声明里用到的单位必须定价
    reserve: { pages: 600 }
    api:
      errors:                          # 按顺序匹配，第一条命中的生效
        - when: { json: msgCode, in: ["A0202", "A0211"] }
          outcome: auth_failed         # Token 错误：隔离这把 Key
        - when: { json: code, in: [-60018, -60019] }
          outcome: rate_limited        # 日额度用完：换 Key，冷却到北京时间次日 00:01
          cooldown_until: { time: "00:01", tz: "+08:00" }
        - when: { json: code, in: [-60012, -60013] }
          outcome: client_error        # 任务不存在：把上游错误体以 404 交给客户端
          status: 404
        - when: { json: code, not_in: [0] }
          outcome: client_error        # 其他业务错误：400
      endpoints:
        - match: "POST /api/v4/extract/task"
          task_id: data.task_id        # 记下任务 ID，固定到这次用的 Key
          usage: { pages: { request_query: pages, default: 600 } }   # 提交时按 ?pages= 计费，没给按 600
        - match: "GET /api/v4/extract/task/{task}"
          pinned_by: task              # 查询只用创建任务的 Key
          usage: {}                    # 查询不计费
```

```bash
# 提交（/rest-idem/ 才会在额度用完时自动换 Key），再轮询
curl -sS -X POST "https://gateway.example.com/rest-idem/mineru-api/api/v4/extract/task?pages=13" \
  -H "Authorization: Bearer $UAIG_TOKEN" -H "Content-Type: application/json" -d '{"url":"https://example.com/a.pdf"}'
curl -sS "https://gateway.example.com/rest/mineru-api/api/v4/extract/task/<task_id>" -H "Authorization: Bearer $UAIG_TOKEN"
```

按字段计费的例子（上游报告了就用上游的，否则按请求推算）：

```yaml
      endpoints:
        - match: "POST /search"
          usage:
            credits: { json: usage.credits, request_json: search_depth, map: { advanced: 2 }, default: 1 }
```

要点：

- **结果类别**：`rate_limited`（换 Key、冷却）、`auth_failed`（隔离 Key）、`overloaded`、`server_error`（换 Key）、`permission_denied`（同 9.5，不隔离）、`client_error`（不换 Key，把上游错误体交给客户端）。
- **字段路径**只支持 `a.b.0.c` 这种写法；值按 JSON 原样比较，`-60018` 与 `"-60018"` 不同，要和上游返回的类型一致。
- **换 Key 与按请求体计费**只在 `/rest-idem/`（以及 `GET`、`DELETE`、`PUT`）上生效；`/rest/` 下的 `POST` 是流式请求体，只尝试一次，也读不到请求字段。工具声明里写 `idempotent: true` 就走 `/rest-idem/`。
- **异步任务在提交时计费**：真实页数要等任务完成才知道，目前按 `?pages=` 或 `default` 结算。
- **重启后照常查询**：任务固定记录与 Key 的冷却状态每 5 秒保存一次，正常关闭时再保存一次（T68），重启前创建的任务重启后仍可查询；进程崩溃时最多丢最近 5 秒的变化。
- 声明了需要读响应体的规则时，JSON 响应会先缓冲再转发（上限 64 MiB）；文件下载等非 JSON 响应仍是流式。

#### 一次请求完成「提交 → 等待 → 取结果」，以及本地文件上传（spec/08 §8.3.7）

端点写 `flow` 时，`match` 是网关上的**虚拟路径**：客户端请求一次，网关用同一把 Key 依次提交、轮询、下载结果。下面三个虚拟端点只用配置实现了 MinerU 的同步解析、本地文件同步解析与本地文件异步上传：

```yaml
pools:
  - name: mineru-rest
    protocol: rest
    base_url: "https://mineru.net"
    pin_ttl: 48h
    timeouts: { response_header_base: 10m }   # 同步等待在发出响应头之前会等很久
    cooldown: { max: 24h }
models:
  - name: mineru-api
    pool: mineru-rest
    price: { pages: "0.01", requests: "0" }
    reserve: { pages: 600 }
    api:
      errors: [ ... ]                           # 同上
      endpoints:
        - match: "POST /parse"                  # POST /rest-idem/mineru-api/parse，请求体 {"url": "..."}
          flow:
            submit: { request: "POST /api/v4/extract/task", task_id: data.task_id }
            poll:
              request: "GET /api/v4/extract/task/{task_id}"
              state: data.state
              done: [done]
              failed: [failed]
            result: { url: data.full_zip_url, zip_entry: full.md, content_type: "text/markdown; charset=utf-8" }
          usage: { pages: { json: data.extract_progress.total_pages, zip_json: { entry: layout.json, count: pdf_info } } }   # 按真实页数计费
        - match: "POST /parse/file"             # POST /rest/mineru-api/parse/file?name=a.pdf，请求体是文件
          flow:
            upload:
              request: "POST /api/v4/file-urls/batch"
              body:
                files.0.name: { query: name, required: true }
                files.0.is_ocr: { query: is_ocr, type: bool }
                language: { query: language }
              upload_url: data.file_urls.0
              task_id: data.batch_id
            poll:
              request: "GET /api/v4/extract-results/batch/{task_id}"
              state: data.extract_result.0.state
              done: [done]
              failed: [failed]
            result: { url: data.extract_result.0.full_zip_url, zip_entry: full.md, content_type: "text/markdown; charset=utf-8" }
          usage: { pages: { json: data.extract_result.0.extract_progress.total_pages, zip_json: { entry: layout.json, count: pdf_info } } }
        - match: "POST /tasks/file"             # 只上传，返回 {"task_id": "..."}，之后用下面的端点查询
          flow:
            upload: { request: "POST /api/v4/file-urls/batch", body: { files.0.name: { query: name, required: true } }, upload_url: data.file_urls.0, task_id: data.batch_id }
          usage: { pages: { request_query: pages, default: 600 } }
        - match: "GET /api/v4/extract-results/batch/{batch}"
          pinned_by: batch
          usage: {}
```

```bash
# 在线文件：一次请求拿到 Markdown（提交 JSON 的 flow 必须走 /rest-idem/）
curl -sS -X POST "https://gateway.example.com/rest-idem/mineru-api/parse" -H "Authorization: Bearer $UAIG_TOKEN" \
  -H "Content-Type: application/json" -d '{"url":"https://example.com/a.pdf"}' -o a.md
# 本地文件：请求体就是文件（上传 flow 必须走 /rest/，--data-binary 会带上 Content-Length）
curl -sS -X POST "https://gateway.example.com/rest/mineru-api/parse/file?name=report.pdf&is_ocr=true" \
  -H "Authorization: Bearer $UAIG_TOKEN" --data-binary @report.pdf -o report.md
```

- **只尝试一次的边界**：提交（或上传完成）之前失败可以换 Key；之后任务已经在上游创建，失败不重试，已见到的页数照常计费。
- **上传**：签名地址只接受定长上传，不支持分块；第三方存储不会收到上游 Key；申请地址的响应（含签名地址）不会返回给客户端。
- **结果**：可以直接返回最后一次查询的 JSON、原样转发下载内容，或从 zip 里取一个文件（单个文件与 zip 都不超过 64 MiB）。
- **页数取不到时**：小文件可能直接完成，轮询中从未出现页数；`zip_json` 从结果包里的文件数出页数（例如 `layout.json` 中 `pdf_info` 的长度），否则按 `reserve` 结算。
- 轮询间隔固定为 2 秒起、每次 ×1.5、最长 15 秒；`poll.timeout` 缺省 10 分钟，最长 1 小时。
- 把虚拟端点声明为工具（9.6）时，`POST /parse` 这类提交 JSON 的接口写 `idempotent: true`，AI 一次调用就能拿到结果。

### 9.9 文件中转：让 AI 把本地文件交给接口（T69，spec/09 §7）

MCP 工具的参数只能是 JSON，AI 没法把本机的 PDF 放进参数里。开启文件中转后，文件**上传一次**得到 `file_…` ID，之后声明了文件参数的工具、以及任何 `/rest/` 请求都可以用这个 ID 代替请求体。

```yaml
files:
  enabled: true
  max_file_bytes: 209715200      # 单个文件 200 MiB（字节数）
  default_ttl: 24h               # 到期自动删除；上传时可以用 ttl 指定，最长 max_ttl（168h）

models:
  - name: mineru-api
    tools:
      - name: mineru_parse_file
        description: "解析一个已上传的 PDF / 图片 / Office 文件，返回 Markdown。先用 gateway_upload_file 上传得到 file_id"
        method: POST
        path: /parse/file                         # 9.8 中的上传类 flow 端点
        file: { arg: file, name_query: name }     # 参数 file 是文件 ID，作为请求体；文件名填进查询参数 name
        input_schema:
          type: object
          properties:
            file:   { type: string, description: "gateway_upload_file 返回的 file_id" }
            is_ocr: { type: boolean }
          required: [file]
```

**AI 怎么上传**：在 AI 所在的机器上用本地 stdio 桥（6.2），并用 `--upload-dir` 指定允许上传的目录（可以写多个）。桥会多提供一个工具 `gateway_upload_file {path, ttl?}`，AI 给出路径即可：

```json
{ "mcpServers": { "uaig": {
  "command": "gateway",
  "args": ["mcp", "--upload-dir", "D:/Docs/to-parse"],
  "env": { "UAIG_URL": "http://127.0.0.1:8080", "UAIG_TOKEN": "uai_live_..." }
} } }
```

AI 先调用 `gateway_upload_file {"path": "D:/Docs/to-parse/报告.pdf"}` 得到 `file_id`，再调用 `mineru_parse_file {"file": "file_…"}`。同一个 ID 在到期前可以反复使用。

- **只允许 `--upload-dir` 里的文件**（解开符号链接后判断）。不要把账号文件、密钥所在的目录加进去；不加 `--upload-dir` 时没有这个工具。
- 命令行：`gateway files upload a.pdf [--ttl 2h]`、`gateway files list`、`gateway files delete <id>`。
- HTTP：`POST /files?name=a.pdf&ttl=2h`（请求体是文件，必须带 `Content-Length`）；之后在请求上加头 `X-UAIG-File: file_…`、请求体留空，例如 `curl -X POST -H "X-UAIG-File: file_…" "$UAIG_URL/rest/mineru-api/parse/file?name=a.pdf"`。
- 文件只属于上传它的客户端，别的客户端拿到 ID 也用不了（与不存在时一样返回 404）。文件不计费；日志只写文件 ID 与大小，不写文件名。
- 上限：单个文件 `max_file_bytes`、每个客户端 `max_files_per_client`（100）与 `max_bytes_per_client`（2 GiB）、全网关 `max_total_bytes`（10 GiB）；超过数量或总量时返回 409，删掉旧文件或等它们到期。管理页面概览显示用量，`GET /admin/api/files` 可以查看每个客户端的用量。
- 文件存放在 `<data_dir>/files`（可以用 `files.dir` 改），重启后保留未到期的文件。不提供下载文件内容的接口。
- 远程 HTTP 方式的 MCP 没有上传工具，用命令行或 `POST /files` 上传后把 ID 告诉 AI。

## 10. MinerU 文档解析

网关对接 MinerU 官方的精准解析 API v4（spec/08 §8.2.1）。配置：

```yaml
pools:
  - name: mineru
    protocol: mineru
    base_url: "https://mineru.net"
    pin_ttl: 48h                       # 异步任务固定到创建它的 Key
    timeouts: { response_header_base: 10m }
    cooldown: { max: 24h }             # 日额度用完的 Key 冷却到北京时间次日
models:
  - name: mineru-parse
    pool: mineru
    price: { pages: "0.01", requests: "0" }
    reserve: { pages: 600 }
```

Key 用 `gateway keys import --pool mineru ...` 导入，写原始 Token（不加 `bearer:`）。

### 10.1 调用方式

| 用途 | 请求 | 响应 |
|:--|:--|:--|
| 在线文件，同步 | `POST /v1/parse`，JSON `{"url": "...", "is_ocr": false, ...}` | Markdown |
| 本地文件，同步 | `POST /v1/parse/file?name=report.pdf`，请求体是文件本身 | Markdown |
| 在线文件，异步 | `POST /v1/tasks`，JSON 同上 | `{"data":{"task_id":"..."}}` |
| 本地文件，异步 | `POST /v1/tasks/file?name=report.pdf` | `{"data":{"task_id":"batch-..."}}` |
| 查询 / 下载结果 | `GET /v1/tasks/{task_id}`、`GET /v1/tasks/{task_id}/result` | 状态 JSON / 结果 zip（未完成时 409） |
| AI 调用 | MCP 工具 `mineru_parse`（参数 `url`） | Markdown |

```bash
# 本地文件同步解析（--data-binary 会带上 Content-Length）
curl -sS -X POST "https://gateway.example.com/v1/parse/file?name=report.pdf&is_ocr=true" \
  -H "Authorization: Bearer $UAIG_TOKEN" --data-binary @report.pdf -o report.md
```

本地文件的解析选项放在查询串里：`name`（必填，带扩展名）、`is_ocr`、`enable_formula`、`enable_table`、`language`、`page_ranges`、`model_version`、`data_id`、`extra_formats`（逗号分隔），`pages`（预计页数，用于预留与异步计费）。

### 10.2 注意事项

1. **计费**：同步解析按实际页数结算；异步提交按 `?pages=` 给出的页数结算，没给时按预留（600 页）结算，需要精确计费请用同步方式或给出页数。
2. **上传不换 Key**：本地文件是流式请求体，只尝试一次；申请上传地址时 Key 失效会直接返回错误，客户端重试即可（调度器已把那把 Key 冷却或隔离）。
3. **必须定长上传**：不支持分块传输（`Transfer-Encoding: chunked`），请求必须带 `Content-Length`。
4. **重启不影响进行中的任务**：任务固定记录与 Key 的冷却状态会保存到数据库（T68），重启（含配置热重载触发的进程内重启）后仍落在创建任务的 Key 上；进程崩溃时最多丢最近 5 秒的变化。
