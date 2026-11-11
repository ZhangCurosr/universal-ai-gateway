# 12 部署（局域网、域名对外、开机自启）

本章面向**部署网关的用户**，同时是任务卡 T15（部署文档与示例文件）的规格。网关本身不内置 TLS，对外提供 HTTPS 一律通过前置反向代理完成。

## 1. 三种部署形态

| 形态 | 数据面监听 | 管理面 | TLS | `trusted_proxies` |
|:--|:--|:--|:--|:--|
| A. 仅本机 | `127.0.0.1:8080` | `127.0.0.1:8081` | 不需要 | `[]` |
| B. 局域网 | `0.0.0.0:8080` | `127.0.0.1:8081` | 可选（可信网络可不加） | `[]` |
| C. 域名对外 | `127.0.0.1:8080`（**只让反向代理访问**） | `127.0.0.1:8081` | 反向代理负责 | 反向代理的地址，如 `["127.0.0.1/32", "::1/128"]` |

| 编号 | 规则 |
|:--|:--|
| DEP-01 | 形态 C 中数据面**必须**只监听回环地址或内网地址，否则攻击者可以绕过反向代理直连网关，并伪造 `X-Forwarded-For` |
| DEP-02 | `trusted_proxies` 只填反向代理自身的地址，**禁止**填 `0.0.0.0/0` 或整段公网 |
| DEP-03 | 管理面在任何形态下都只监听回环地址（CFG-02 强制）。远程管理用 SSH 隧道：`ssh -L 8081:127.0.0.1:8081 user@gateway-host` |

## 2. 局域网接入（形态 B）

1. 配置 `server.data_listen: "0.0.0.0:8080"`。
2. 在网关所在机器放行入站 TCP 8080：
   - Windows：`New-NetFirewallRule -DisplayName "AI Gateway" -Direction Inbound -Protocol TCP -LocalPort 8080 -Action Allow -Profile Private`（只放行「专用网络」配置文件，不放行公用网络）
   - Linux（ufw）：`sudo ufw allow from 192.168.0.0/16 to any port 8080 proto tcp`
3. 局域网客户端：
   - Claude Code：`ANTHROPIC_BASE_URL=http://<网关IP>:8080`、`ANTHROPIC_AUTH_TOKEN=uai_live_...`
   - OpenAI SDK：`base_url="http://<网关IP>:8080/v1"`
4. 风险提示：局域网内是明文 HTTP，同网段设备可以嗅探到 Token。公共或不可信网络请改用形态 C（可以是内网域名 + 自签证书）。

## 3. 域名对外（形态 C）

### 3.1 Caddy（推荐，自动申请与续期 HTTPS 证书）

```caddyfile
gateway.example.com {
    request_body {
        max_size 32MB            # 与 limits.max_request_body_bytes 保持一致
    }
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1        # 立即转发 SSE，不缓冲
        transport http {
            read_timeout  15m    # 长流式响应
            write_timeout 15m
        }
    }
}
```

Caddy 默认会追加 `X-Forwarded-For`，网关配置 `trusted_proxies: ["127.0.0.1/32", "::1/128"]` 即可。

### 3.2 nginx

```nginx
server {
    listen 443 ssl http2;
    server_name gateway.example.com;
    ssl_certificate     /etc/letsencrypt/live/gateway.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/gateway.example.com/privkey.pem;

    client_max_body_size 32m;            # 与 limits.max_request_body_bytes 一致

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        proxy_buffering off;             # 必须：否则 SSE 流会被攒成整块再发送
        proxy_request_buffering off;
        proxy_read_timeout 900s;         # 长流式响应
        proxy_send_timeout 900s;
    }
}
```

### 3.3 内网穿透 / 无公网 IP

使用 Cloudflare Tunnel、frp 等时，网关看到的 `RemoteAddr` 是隧道客户端的本机地址：把隧道客户端所在地址（通常是 `127.0.0.1/32`）加入 `trusted_proxies`。注意部分隧道服务对单个请求时长有限制，长流式请求可能被截断，部署前用一个长输出的请求实测。

### 3.4 新端点与 AI 出口的代理要点

上面两个示例用 `location /` / 整站反代，`/v1/responses*`、`/mcp` 与上传端点都会被转发，无需单独配置路径。需要注意的差异：

| 场景 | 要求 |
|:--|:--|
| `/mcp`（spec/14） | 必须原样转发 `Authorization`、`MCP-Protocol-Version`、`Origin` 头（Caddy 与上例 nginx 默认都会转发）。服务端只回 JSON、不用 SSE 与 WebSocket，无需 `Upgrade` 配置。浏览器类客户端经代理访问时 `Origin` 是公网域名，会被 SRV-M06 拒绝——M1 只支持非浏览器 MCP 客户端 |
| `/v1/responses` 流式 | 与其他 SSE 相同：nginx `proxy_buffering off`、Caddy `flush_interval -1` |
| 文件上传（`BodyStream` 路由，如 MinerU） | 代理的请求体上限要放到 `limits.max_stream_body_bytes`，但只对上传路径放开：nginx 为该路径单独写 `location` 并设 `client_max_body_size 512m; proxy_request_buffering off;`（否则 nginx 会先把整个文件落盘再转发）；Caddy 在对应 `handle_path` 中单独设 `request_body { max_size 512MB }`。网关的 `server.read_timeout` 要按「文件大小 / 最低上行带宽」调大 |
| 同步封装的长任务（spec/08 §8.2） | 代理的读超时（nginx `proxy_read_timeout`、Caddy `read_timeout`）必须 ≥ 该池的 `timeouts.response_header_base`，否则代理先断开，上游的费用照样产生 |
| 只想开放部分能力 | 用代理按路径放行（例如只放 `/v1/` 与 `/mcp`），其余返回 404；网关自身的白名单（`allowed_models`）仍然生效 |

### 3.5 对外前检查清单

- [ ] DEP-01：`data_listen` 是 `127.0.0.1:8080`，从公网直连 8080 不通。
- [ ] `trusted_proxies` 只含反向代理地址；从外部带伪造的 `X-Forwarded-For` 请求，访问日志中的 `client_ip` 仍是真实来源。
- [ ] 反向代理的请求体上限与 `max_request_body_bytes` 一致。
- [ ] 流式请求能逐字输出（不是等结束才一次性出现）。
- [ ] `/mcp` 可用：`UAIG_URL=https://域名 UAIG_TOKEN=... gateway tools` 能列出工具；不带 Token 返回 401。
- [ ] 如果接入了上传类 API：用接近上限的大文件实测一次，确认代理与 `read_timeout` 不会中途断开。
- [ ] `limits.preauth_ip_rpm` 已启用（默认 600）。
- [ ] 每个客户端都设置了额度与 `rpm_limit`；没有给普通客户端 `quota_unlimited`。
- [ ] `master_key_file` 不在数据目录下，且已离线备份（丢失主密钥 = 所有上游 Key 无法解密）。

## 4. 资源与调参

| 机器内存 | `max_in_flight_requests` | `max_buffered_body_bytes` | `max_request_body_bytes` |
|:--|:-:|:-:|:-:|
| 1 GiB | 64 | 128 MiB | 16 MiB |
| 2 GiB | 128 | 256 MiB | 32 MiB |
| ≥ 4 GiB | 256（默认） | 512 MiB（默认） | 32 MiB（默认） |

内存估算见 spec/09 §5.1：请求体相关内存约 `3 × max_buffered_body_bytes`，原始缓冲响应另外共享一份同大小预算，每份响应仍有 64 MiB 上限。切片扩容、JSON 解析、转译与 MCP 结果有额外开销，这些预算不是精确的进程堆上限。

上游不自动跟随 HTTP 重定向（HDR-06），池的 `base_url` 必须直接指向 API 地址；依赖跳转的下载接口会返回错误。客户端写超时复用池的 `stream_idle` 并在每次写入时续期，停止读取的客户端会被终止，正常长流不受固定总写时长限制。

实测值（T50）：在 Linux x86_64 环境下以并发长流式压测（每路 10ms/chunk 持续 5s），实测结果对照如下：
- **默认档（≥ 4 GiB，256 路并发）**：Heap 峰值 **84.03 MiB**，系统内存 Sys 峰值 **116.56 MiB**，协程峰值 2322，结束后无泄漏回落至 17。
- **1 GiB 档（64 路并发）**：Heap 峰值 **97.48 MiB**，系统内存 Sys 峰值 **120.56 MiB**，协程峰值 594，结束后无泄漏回落至 17。
实测值远低于理论峰值上限，证实流式管道无内存积压。完整测量数据见 `docs/handoff/reports/T50_perf_baseline.md`。

## 5. 开机自启与崩溃自动重启

网关是单进程；进程退出时服务中断，必须交给系统服务管理器自动拉起。

强制结束进程会丢失队列、批次与重试缓冲中的全部未持久化计费事件；`flush_interval` 不是丢失窗口的上界。正常关闭会最终刷盘并在失败时尝试写入 `unflushed_usage.jsonl`，下次启动恢复，详见 spec/06 §5。

### 5.1 Linux（systemd）

`deploy/systemd/universal-ai-gateway.service`（T15 提供该文件）：

```ini
[Unit]
Description=Universal AI Gateway
After=network-online.target
Wants=network-online.target

[Service]
User=uaig
WorkingDirectory=/opt/uaig
ExecStart=/opt/uaig/bin/gateway serve --config /opt/uaig/gateway.yaml
Restart=on-failure
RestartSec=3
TimeoutStopSec=40            # 大于 shutdown_grace（30s），留出刷盘时间
LimitNOFILE=65536
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/uaig/data /opt/uaig/secrets

[Install]
WantedBy=multi-user.target
```

### 5.2 Windows

M1 的二进制不是 Windows 服务程序，使用「任务计划程序」在开机时运行：

```powershell
$action  = New-ScheduledTaskAction -Execute "C:\uaig\bin\gateway.exe" -Argument "serve --config C:\uaig\gateway.yaml" -WorkingDirectory "C:\uaig"
$trigger = New-ScheduledTaskTrigger -AtStartup
$set     = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName "UniversalAIGateway" -Action $action -Trigger $trigger -Settings $set -User "SYSTEM" -RunLevel Highest
```

（需要管理员权限运行；`-User SYSTEM` 时数据目录与密钥目录要对 SYSTEM 可写。）

## 6. 任务卡 T15 的交付物

| 文件 | 内容 |
|:--|:--|
| `deploy/Caddyfile.example` | §3.1 |
| `deploy/nginx.conf.example` | §3.2 |
| `deploy/systemd/universal-ai-gateway.service` | §5.1 |
| `deploy/windows/install-task.ps1` | §5.2，带参数 `-InstallDir`，脚本开头检查管理员权限 |
| `docs/DEPLOYMENT.md` | 面向用户的中文部署指南：§1 ~ §5 的精简版；客户端接入示例照搬 spec/14 §4（Claude Code、Codex CLI、OpenAI SDK 作为模型后端；MCP 远程与 stdio；`gateway call` 命令行） |

## 7. 版本与发布（T50）

| 编号 | 规则 |
|:--|:--|
| REL-01 | 版本号用语义化版本 `vMAJOR.MINOR.PATCH`，以 git 标签记录。M3 的首个版本为 `v0.3.0`（M1 = 0.1、M2 = 0.2、M3 = 0.3） |
| REL-02 | 发布构建一律 `CGO_ENABLED=0`、`-trimpath`、`-ldflags "-s -w -X main.version=<版本>"`。`<版本>` 取 `git describe --tags --always --dirty`；仓库没有任何标签时为 `v0.0.0-<短提交号>`（工作树不干净时再加 `-dirty`） |
| REL-03 | 目标平台：`linux/amd64`、`linux/arm64`、`windows/amd64`、`darwin/arm64` |
| REL-04 | 产物文件名 `universal-ai-gateway_<版本>_<GOOS>_<GOARCH>`，Windows 加 `.exe`；放在 `dist/`（已加入 `.gitignore`）；同目录生成 `SHA256SUMS`，每行 `<64 位小写十六进制>  <文件名>`（两个空格，与 `sha256sum` 一致），按文件名排序 |
| REL-05 | 发布工具 `go run ./tools/release` 在构建前检查：工作树干净（`git status --porcelain` 为空）、`go run ./tools/check` 通过。任何一项不满足就以非零码退出，不产出任何文件。`-allow-dirty` 跳过工作树检查，仅供本地试验 |
| REL-06 | 发布工具不创建标签、不推送、不上传。打标签与发布由用户手动完成 |
| REL-07 | 构建完成后，对与当前机器 `GOOS/GOARCH` 相同的产物执行 `<产物> version`，输出必须等于 REL-02 的版本号，否则失败；当前平台不在 REL-03 清单内时跳过并打印说明 |
| REL-08 | 发布工具调用的每个外部命令都有超时：`tools/check` 最长 15 分钟，单个平台构建最长 5 分钟 |
