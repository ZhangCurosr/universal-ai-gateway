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
| 文件上传（如 MinerU 的 `/v1/parse`、`/v1/tasks`） | 只对这两个路径把上限放宽到 `limits.max_stream_body_bytes`（默认 512 MiB）；nginx 还要关闭 `proxy_request_buffering`。网关的 `server.read_timeout` 按「文件大小 ÷ 最低上行带宽」调大 |
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

峰值内存约为 `3 × max_buffered_body_bytes`，加上每个非流式请求最多 64 MiB 的响应缓冲，再加约 40 MiB 的运行时开销。调小 `max_request_body_bytes` 时，反向代理的上限要一起调小。

## 5. 开机自启与崩溃自动重启

网关是单进程，进程一退出服务就中断，所以要交给系统的服务管理器自动拉起。

**Linux（systemd）**：使用 [deploy/systemd/universal-ai-gateway.service](../deploy/systemd/universal-ai-gateway.service)，文件开头的注释里有安装步骤。`TimeoutStopSec=40s` 比 `shutdown_grace`（默认 30s）大，给关闭时排空在途请求和计费刷盘留出时间。如果调大了 `shutdown_grace`，这个值要一起调大。首次启动后，用 `journalctl -u universal-ai-gateway` 查看引导 Token。

**Windows（任务计划程序）**：

1. 先在前台运行一次 `C:\uaig\bin\gateway.exe serve --config C:\uaig\gateway.yaml`，记下引导 Token，然后按 Ctrl+C 退出。
2. 以管理员身份打开 PowerShell，执行：
   ```powershell
   .\deploy\windows\install-task.ps1 -InstallDir C:\uaig
   ```
   脚本会先校验配置，再注册一个开机时以 SYSTEM 身份运行、退出后每分钟重启的计划任务。
3. 以 SYSTEM 身份运行时，`data_dir` 和 `master_key_file` 所在的目录必须对 SYSTEM 可写。

## 6. 客户端接入

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
