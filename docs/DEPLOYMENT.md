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
