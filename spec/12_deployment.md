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

### 3.4 对外前检查清单

- [ ] DEP-01：`data_listen` 是 `127.0.0.1:8080`，从公网直连 8080 不通。
- [ ] `trusted_proxies` 只含反向代理地址；从外部带伪造的 `X-Forwarded-For` 请求，访问日志中的 `client_ip` 仍是真实来源。
- [ ] 反向代理的请求体上限与 `max_request_body_bytes` 一致。
- [ ] 流式请求能逐字输出（不是等结束才一次性出现）。
- [ ] `limits.preauth_ip_rpm` 已启用（默认 600）。
- [ ] 每个客户端都设置了额度与 `rpm_limit`；没有给普通客户端 `quota_unlimited`。
- [ ] `master_key_file` 不在数据目录下，且已离线备份（丢失主密钥 = 所有上游 Key 无法解密）。

## 4. 资源与调参

| 机器内存 | `max_in_flight_requests` | `max_buffered_body_bytes` | `max_request_body_bytes` |
|:--|:-:|:-:|:-:|
| 1 GiB | 64 | 128 MiB | 16 MiB |
| 2 GiB | 128 | 256 MiB | 32 MiB |
| ≥ 4 GiB | 256（默认） | 512 MiB（默认） | 32 MiB（默认） |

峰值内存估算见 spec/09 §5.1：约 `3 × max_buffered_body_bytes` + 每个非流式请求最多 64 MiB + 运行时基础约 40 MiB。

## 5. 开机自启与崩溃自动重启

网关是单进程；进程退出时服务中断，必须交给系统服务管理器自动拉起。

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
| `docs/DEPLOYMENT.md` | 面向用户的中文部署指南：§1 ~ §5 的精简版，含 Claude Code / OpenAI SDK 接入示例 |
