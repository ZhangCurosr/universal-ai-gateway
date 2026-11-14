---
name: operate-gateway
description: 运维一个正在运行的 universal-ai-gateway：补充与同步上游 Key、查看池的剩余额度与预计用完时间、处理池级告警、停用失效的 Key、给新设备开放（局域网共享与客户端 Token）、新建池与服务、重载配置、排查「接不上」。用户问「Key 怎么同步」「池子还够用吗」「告警了怎么办」「另一台电脑怎么用」「加一个池」时使用。不用于修改网关的代码。
---

# 运维 universal-ai-gateway

所有命令都在能访问网关管理面的机器上运行（管理面只监听本机，缺省 `http://127.0.0.1:8081`）。完整说明在网关仓库的 `docs/DEPLOYMENT.md`；本技能按场景给出步骤。

## 规则

1. **管理员 Token 只放在环境变量 `UAIG_ADMIN_TOKEN`**，不写进文件、命令行参数或回复；账号文件（含 Key、邮箱、密码）只把路径交给 `gateway keys import / sync`，不要打开、打印或复述内容。
2. **会修改网关的操作先征得用户同意**：`gateway admin call` 的写入类工具（`set_key_state`、`update_client`、`reload`）必须加 `--yes`，加之前把要做的修改告诉用户；`keys sync` 会停用 Key，先不加 `--watch` 跑一次给用户看结果。
3. 查询用只读命令：`gateway admin tools` 列出工具，`read` 类随时可用。

准备：

```bash
export UAIG_ADMIN_URL=http://127.0.0.1:8081 UAIG_ADMIN_TOKEN=<由用户设置>
```

（Windows PowerShell：`$env:UAIG_ADMIN_URL = "http://127.0.0.1:8081"`；Token 由用户自己设置。）

## 场景

### 池子还够用吗

```bash
gateway admin call pools          # 每个池：可用 Key 数、本期已用 / 剩余额度、每小时消耗、runs_out_at 或 lasts_until_reset
gateway admin call alerts         # 正在触发的告警：low_keys / low_quota / expiring
gateway admin call key_stats --args '{"pool":"<池>"}'   # 每把 Key 的请求数、用量、24 小时失败率
```

回答时说清楚：还剩多少、按最近 24 小时的速度能撑到什么时候、是否能撑到下次重置。

### 补充或同步 Key（密钥不经过对话）

- 一次性导入：`gateway keys import --pool <池> --file <账号文件路径> --where status=ok --name-prefix <前缀>`（CSV 加 `--format csv`；有过期时间加 `--expires-field expires_at --tz +08:00`）。先加 `--dry-run` 看计数。
- 让池跟着账号文件走（新增、更新，停用文件中已经没有或失效的账号）：`gateway keys sync --pool <池> --file <路径> --label <来源标签> ...`；一次要停用超过一半时会拒绝，确认文件没问题再加 `--force`；常驻同步加 `--watch 10m`。
- 网页：「上游 Key」页 →「批量导入」（只导入，不停用）。

### 处理告警或失效的 Key

1. `gateway admin call alerts` 看是哪个池、哪类告警。
2. `low_keys` / `low_quota`：补 Key（上一节），或确认额度重置时间（`pools` 的 `next_reset`）。
3. 某把 Key 持续失败：`key_stats` 找到失败率高、`last_failure` 为 `auth_failed` / `permission_denied` 的 Key，告诉用户后停用：
   `gateway admin call set_key_state --args '{"id":<id>,"state":"disabled"}' --yes`

### 给另一台设备或另一个项目用

1. 网关所在机器：管理页面「局域网共享」查看是否开放、本机的局域网地址、防火墙命令（用户自己在管理员 PowerShell 中运行）。
2. 管理页面「客户端」为那台设备新建 Token（只显示一次，由用户自己复制到那台设备）。
3. 那台设备：设置 `UAIG_URL=http://<网关局域网 IP>:8080` 与 `UAIG_TOKEN`，在项目里运行 `gateway connect <claude|codex|opencode|pi> --write`。
4. 接不通：在那台设备上运行 `gateway doctor`（若该版本已提供），或依次检查 `curl $UAIG_URL/healthz`、`curl $UAIG_URL/v1/models -H "Authorization: Bearer $UAIG_TOKEN"`、网关的局域网共享与防火墙。

### 新建池与服务、修改配置

- 网页：「配置」→「池与服务」（表单 + YAML，写回 `gateway.yaml` 并热重载）。
- 接入新的 REST API：用 `add-api` 技能。
- 手工改了 `gateway.yaml`：先 `gateway check-config --config gateway.yaml`，再 `gateway reload --dry-run` 看会改什么（热重载还是需要重启），用户同意后 `gateway reload`。

### 用量与费用

```bash
gateway admin call usage                                   # 最近 24 小时，按客户端与服务
gateway admin call usage --args '{"from":"2026-11-01T00:00:00Z","to":"2026-11-15T00:00:00Z"}'
gateway admin call list_clients                            # 客户端的额度与已花费
```

调整客户端额度或速率：`gateway admin call update_client --args '{"id":<id>,"quota_nano_usd":<纳美元>}' --yes`（1 美元 = 1000000000），先告诉用户。

## 不能做的（告诉用户去管理页面或手工处理）

- 新建客户端、轮换 Token、手工添加单把 Key：会把凭据交给 AI，管理面 MCP 与命令行有意不提供。
- 管理员 Token 丢失：网页与 API 都找不回，需要另一个管理员轮换；建议用户保留两个管理员客户端。
