# T78 `gateway doctor`：一条命令诊断「为什么接不上」（已批准）

- **类型**：规格 + 实现。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：T73（`gateway connect`，`DONE`）。
- **约束**：只读：不改任何文件、不调用会计费的接口；输出中不出现 Token 的值；不加依赖。

## 1. 问题

另一台设备或另一个项目接网关失败时，原因可能在很多地方：环境变量没设、地址写错、网关没开局域网共享、防火墙拦截、Token 无效或被停用、Token 没有某个服务的权限、某个 CLI 的配置没写对（实测中遇到 Pi 读的是 `PI_CODING_AGENT_DIR` 下的 `models.json`）。AI 只能一项一项猜。

## 2. 命令

```
gateway doctor [--cli claude|codex|opencode|pi|all] [--json]
```

按顺序检查，每项输出一行 `通过 / 警告 / 失败 + 原因 + 怎么修`，遇到失败后，依赖它的检查标为「跳过」：

| 编号 | 检查 | 失败时的提示 |
|:--|:--|:--|
| DR-01 | `UAIG_URL` 是否设置、格式是否为 http(s)；`UAIG_TOKEN` 是否设置、是否以 `uai_live_` 开头 | 如何设置环境变量（Windows `setx`、其他写进 shell 配置） |
| DR-02 | `GET /healthz`（5 秒超时）：连不上时区分「拒绝连接」「超时」「DNS 失败」；地址是局域网 IP 时提示检查网关的「局域网共享」与防火墙 | 对应的原因与处理办法 |
| DR-03 | `GET /v1/models`：401 → Token 无效或被停用；返回空列表 → 这个 Token 没有任何服务的权限（`allowed_models`）；否则列出服务数 | 请管理员检查客户端 |
| DR-04 | MCP：`initialize` + `tools/list`，列出工具数与是否为目录模式 | — |
| DR-05 | `--cli`：按 `gateway connect` 的路径规则（含 `CODEX_HOME`、`XDG_CONFIG_HOME`、`PI_CODING_AGENT_DIR`、`CLAUDE_CONFIG_DIR`）检查该 CLI 的配置是否存在、地址是否与 `UAIG_URL` 一致、是否引用 `UAIG_TOKEN`（而不是写死了 Token）、`use-gateway` 技能是否已安装及版本是否与当前 `gateway` 一致；Claude Code 另查 `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` | 运行 `gateway connect <cli> --write` |
| DR-06 | 配置文件里**写死了 Token 的值**（而不是引用变量）→ 警告，建议改为引用 `UAIG_TOKEN` | — |
| DR-07 | `--json`：`{"checks": [{"id", "status": "pass|warn|fail|skip", "message", "fix"}], "ok": bool}`；退出码：有失败为 1，只有警告为 0 | — |

## 3. 改动文件

- 新增 `cmd/gateway/doctor.go` 与测试（复用 `connect.go` 的路径规则与 `mcp.Client`）；`main.go` 用法。
- 技能：`use-gateway` 增加「接不通先运行 `gateway doctor`」；`operate-gateway` 的排障一节引用它。
- 规格：spec/14 新增 §8（DR-01 ~ DR-07）；spec/11 T78；`docs/AI_QUICKSTART.md` §8 加一句。

## 4. 测试

- 每项检查的通过 / 失败分支（假网关：拒绝连接、401、空服务列表、正常）；失败后的跳过；CLI 配置的四种情况（不存在、地址不一致、写死 Token、正常）；输出中没有 Token；`--json` 与退出码。

## 5. 实测（验收）

在本机：未设变量、网关未启动、错误 Token、正确配置四种情况各运行一次；用 `gateway connect` 配好的四个 CLI 各运行一次 `--cli`。

## 6. 待决策

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 是否实际调用一次模型验证 | A. 不调用（只读、不计费）；B. 加 `--ping <服务名>` 选项，调用一次 | **A**，以后需要再加 B：诊断本身不应产生费用 |
| 2 | 发现 CLI 配置有问题时是否自动修复 | A. 只给出修复命令（`gateway connect … --write`）；B. 自动修复 | **A**：`doctor` 保持只读，修改交给 `connect`，职责清楚 |
