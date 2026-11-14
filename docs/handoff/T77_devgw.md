# T77 一次性测试网关 `tools/devgw` 与内置假上游（已批准）

- **类型**：开发工具（不进入 `gateway` 程序与发布包）。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：无。
- **约束**：放在 `tools/` 下（与 `tools/check`、`tools/skillsync` 同级），不被 `internal/` 或 `cmd/` 导入；不加依赖；只监听 `127.0.0.1`；从不读取账号文件。

## 1. 问题

每次实测都要手工重复：把网关编译到临时目录、写配置、在后台启动、从日志里找一次性的引导管理员 Token、测完停进程并删除 `data`、`secrets`、日志。这次会话里出过的问题：

- 重启后引导 Token 已经被日志覆盖，只能删数据重来；
- 没有大模型的真实 Key 时，假的 OpenAI / Anthropic 上游写了两遍（Python），流式事件格式还要现查；
- 停进程要记得 `taskkill //F`，否则旧进程残留占着端口。

## 2. 命令

```
go run ./tools/devgw --config <yaml> [--fake openai,anthropic,responses] [--dir <临时目录>] [--keep]
```

1. 编译 `./cmd/gateway` 到临时目录（`--dir` 缺省为系统临时目录下的新目录），把配置复制进去；配置里的 `data_dir`、`master_key_file` 改指向这个目录（不改用户的文件）。
2. `--fake` 指定的假上游在 `127.0.0.1` 的随机端口启动；配置中 `base_url` 写成 `fake:openai`、`fake:anthropic`、`fake:responses` 的池自动替换为对应地址。
3. 启动网关，等 `/healthz` 返回 200；把引导管理员 Token 写到 `<目录>/admin.token`（权限 0600），再创建一个不限额的测试客户端，Token 写到 `<目录>/client.token`。终端只打印地址、两个 Token 文件的路径和可直接复制的环境变量设置命令（`UAIG_URL`、`UAIG_ADMIN_URL`，Token 用 `$(cat …)` 引用），不打印 Token 本身。
4. 前台运行，`Ctrl+C` 时停止网关与假上游，删除整个目录（`--keep` 保留，便于事后看日志）。

## 3. 假上游

| 协议 | 端点 | 行为 |
|:--|:--|:--|
| openai | `POST /v1/chat/completions` | 非流式与流式（含 `stream_options.include_usage` 的用量块），回答内容见下 |
| anthropic | `POST /v1/messages`、`/v1/messages/count_tokens` | 非流式与流式（`message_start` … `message_stop`） |
| responses | `POST /v1/responses` | 非流式与流式（`response.created` … `response.completed`，message 项带 `content: []`） |

- 回答内容缺省为 `pong`；请求的最后一条用户消息以 `echo:` 开头时原样回显其余部分；带工具定义且最后一条消息包含 `call:<工具名>` 时返回一次工具调用（参数 `{}`），用于测试工具调用路径。
- 每个请求在 `<目录>/fake.log` 记一行：路径、模型、是否流式、消息角色序列、工具数（不记内容）。T75 的问题就是靠角色序列定位的。

## 4. 与现有内容的关系

- `live-test` 技能的「步骤 1 ~ 3」改为运行 `devgw`；没有真实 Key 时用 `--fake`。
- 网关自己的端到端测试继续用 Go 内置的 `httptest` 假上游，不依赖 `devgw`。

## 5. 测试

- `tools/devgw` 的假上游：三种协议的流式与非流式响应能被网关对应的出站适配器解析（用 `httptest` 起假上游，经网关转发后检查结果）。
- 配置改写：`data_dir`、`master_key_file`、`fake:` 地址替换，原文件不变。
- Token 文件权限、`--keep`、退出时清理（Windows 与类 Unix 都要能停掉子进程）。

## 6. 待决策

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 放在哪里 | A. `tools/devgw`（开发工具，不进发布包）；B. 做成 `gateway dev` 子命令 | **A**：只给开发与实测用，不应出现在用户的程序里；假上游也不该打包进去 |
| 2 | 假上游覆盖哪些协议 | A. openai、anthropic、responses 三种；B. 再加 gemini、tavily、mineru | **A**：大模型 CLI 的兼容问题都出在这三种；搜索与文档解析有真实 Key，可以直接实测 |
| 3 | Token 怎么交给使用者 | A. 写到权限 0600 的文件，终端只给路径；B. 打印到终端 | **A**：终端输出常被复制进对话或日志 |
