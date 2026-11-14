# T74 管理面命令行：`gateway admin tools` / `gateway admin call`（已批准，已完成）

- **类型**：规格 + 实现（小卡）。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：T58（管理面 MCP，`DONE`）。
- **约束**：不改 `internal/core` 与 `internal/protocol` 的导出 API；不加依赖；不新增管理 API（复用 `/admin/mcp` 的 13 个工具）。

## 1. 要解决的问题

查询运行中网关的池、Key 统计、告警、重载记录，现在只能通过管理面 MCP（`gateway admin-mcp`）或手写 HTTP 请求。Pi 不内置 MCP；Codex、OpenCode 每个项目都要单独配 MCP。开发本项目时，AI 也经常要「看一眼网关现在的状态」。

本卡为管理面 MCP 的每个工具提供命令行入口，任何能执行 shell 命令的 AI CLI 都能用。数据面已经有同样的 `gateway tools` / `gateway call`（CLI-06、CLI-07），本卡与它们保持一致。

## 2. 命令

```
gateway admin tools [--json]
gateway admin call <工具> [--args JSON|-] [--json] [--yes]
```

- 连接信息与 `gateway admin-mcp` 相同：`UAIG_ADMIN_URL`（缺省 `http://127.0.0.1:8081`）、`UAIG_ADMIN_TOKEN`（必填），不提供 `--token` 参数。
- `<工具>` 可以写全名（`gateway_admin_pools`），也可以省略前缀（`pools`、`key_stats`、`alerts`）。

## 3. 规则

| 编号 | 规则 |
|:--|:--|
| AC-01 | `admin tools`：经 `/admin/mcp` 调用 `tools/list`，每行 `短名  [只读|写入]  说明首行`；`--json` 输出完整列表 |
| AC-02 | `admin call`：经 `/admin/mcp` 调用 `tools/call`；结果文本写到 stdout（管理面工具返回 JSON 文本，原样输出，便于 `jq` 处理）；`isError` 时写到 stderr，退出码 1；`--json` 输出完整的 CallToolResult |
| AC-03 | 写入类工具（`readOnlyHint: false`：`update_client`、`set_key_state`、`reload`）必须加 `--yes`，否则退出码 2，提示「这会修改网关：<工具名> <参数>，确认后加 --yes」。`reload` 的 `{"dry_run": true}` 视为只读，不需要 `--yes` |
| AC-04 | 输出与错误中不出现管理员 Token（沿用 `redactToken`）；管理面工具本身不返回 Key 明文（AM-04 不变） |
| AC-05 | 未知工具 → 退出码 2，列出可用的短名 |

## 4. 改动文件

- `cmd/gateway/client_cmds.go`：`cmdAdmin`（`tools`、`call` 两个子命令；复用 `cmdTools` / `cmdCall` 的输出逻辑，抽出共用函数）；`main.go` 用法；`client_cmds_test.go` 增加用例。
- 技能：`.agents/skills/live-test/SKILL.md`「常用验证点」改用 `gateway admin call pools` 等；`task-card` 不变。
- 规格：spec/14 §6 增加 AC-01 ~ AC-05；spec/11 新增 T74；`docs/DEPLOYMENT.md` 管理面 MCP 一节补充命令行用法。

## 5. 测试

- 用假管理面 MCP 服务：`tools` 输出、短名与全名、`--json`、只读调用、写入调用没有 `--yes`（不发请求）、有 `--yes`、`reload` 的 dry-run、`isError` 退出码、未知工具、输出中没有 Token。
- 端到端：真实网关上 `gateway admin call pools`、`admin call set_key_state --args '{"id":1,"state":"disabled"}' --yes`。

## 6. 待决策

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 写入类工具要不要额外确认 | A. 必须加 `--yes`；B. 不需要 | **A**：AI 在 shell 里调用时多一道明确的意图表达，误操作（停用 Key、改客户端额度、重载）代价高 |
| 2 | 是否支持省略 `gateway_admin_` 前缀 | A. 支持短名；B. 只认全名 | **A**：命令行更好写；全名仍然可用，不冲突 |
| 3 | 命令名 | A. `gateway admin tools` / `admin call`（与数据面的 `tools` / `call` 对应）；B. 每个工具一个子命令（`gateway admin pools`） | **A**：工具列表来自服务端，新增工具不用改命令行；B 每加一个工具都要改代码 |
