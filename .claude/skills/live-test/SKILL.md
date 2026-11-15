---
name: live-test
description: 用真实上游 Key 对 universal-ai-gateway 做本机实测（验收任务卡、复现线上问题、验证配置），全程不泄露密钥：网关跑在临时目录，账号文件只按路径交给本机进程，脚本只输出计数与状态，测完清理。用户说「实测」「用真实 Key 测一下」「验收 Txx」时使用。
---

# 用真实 Key 安全实测

## 安全规则（任何一条都不能违反）

1. **账号文件只按路径使用**：不要用 cat / head / Read / grep 查看它的内容；只把路径交给 `gateway keys import` / `keys sync`，或交给**只输出计数或字段名**的脚本。
2. 脚本输出、日志、回复里不出现 Key、客户端 Token、邮箱、密码、手机号。脚本里收集这些敏感值，最后检查输出与网关日志中一个都没有（见 `livetest_template.py` 的 `leaks()`）。
3. 只上传本地生成的测试文件，不上传用户的个人文件；Webhook 只推给本机的假接收端。
4. 浏览器里登录管理页面只用本机测试网关的引导 Token（启动日志里那一次）。
5. 测完：停掉 `devgw`（它会停网关并删除目录）；用了 `--keep` 或没用 `devgw` 时，停掉网关（Windows 用 `taskkill //F //IM gateway.exe`，不加 `/F` 旧进程会留着），删除临时目录里的 `data`、`secrets`、`gw*.log`、Token 文件。

## 步骤

1. **写配置**：监听 `127.0.0.1:18496`（数据面）/ `127.0.0.1:18497`（管理面）。先 `go run ./cmd/gateway check-config --config x.yaml`：它做与启动相同的完整检查（T76），缺少价目单位（如 `openai_chat` 的 `cache_read_tokens`、`mineru` 的 `requests`）、接口声明用错协议、告警地址文件读不到都会在这里报出。
   没有大模型的真实 Key 时，把池的 `base_url` 写成 `fake:openai`、`fake:anthropic` 或 `fake:responses`（协议分别为 `openai_chat`、`anthropic`、`openai_responses`），用内置假上游。
2. **用 `devgw` 启动一次性网关**（T77，在后台运行，输出写文件）：
   ```bash
   go run ./tools/devgw --config x.yaml [--fake openai,anthropic,responses] [--keep] > devgw.out 2>&1
   ```
   它把网关编译进新的临时目录，改写配置副本（`data_dir`、`master_key_file` 指向该目录，监听只在 `127.0.0.1`），等 `/healthz` 就绪，
   把引导管理员 Token 写到 `<目录>/admin.token`、一个不限额测试客户端的 Token 写到 `<目录>/client.token`（`gateway.log` 中那一行已替换）；
   指向假上游的池自动加一个占位 Key。`devgw.out` 只有地址、文件路径和设置环境变量的命令（Token 用 `$(cat …)` 引用）。
   假上游回答 `pong`；最后一条用户消息以 `echo:` 开头时回显其余部分；带工具定义且消息包含 `call:<工具名>` 时返回一次工具调用。
   `<目录>/fake.log` 每个请求一行：路径、模型、是否流式、角色序列、工具数（查转译问题用）。
3. **停止**：前台按 `Ctrl+C`；后台运行时创建 `<目录>/stop` 文件（`touch <目录>/stop`；不要直接杀 `devgw` 进程，网关子进程会残留）。它会停掉网关与假上游并删除整个目录；`--keep` 保留目录看日志（之后自己删）。
4. **导入 Key**（输出只有计数）：
   ```bash
   UAIG_ADMIN_URL=http://127.0.0.1:18497 UAIG_ADMIN_TOKEN="$(cat <目录>/admin.token)" \
     <目录>/gateway.exe keys import --pool <池> --file <账号文件路径> --where status=ok --name-prefix <前缀>
   ```
   CSV 加 `--format csv`；有过期时间加 `--expires-field expires_at --tz +08:00`。
5. **跑实测脚本**：复制 `livetest_template.py`，按卡片的验收步骤写 `check(...)`；每一项打印 `PASS` / `FAIL` 与不含敏感值的细节，最后打印 `N/M passed`。耗时长的放后台运行，输出写文件。
6. **浏览器验证 UI**（需要时）：打开 `http://127.0.0.1:18497/admin/ui/`，用 `admin.token` 里的引导 Token 登录；页面每 10 秒刷新，元素引用会失效，点击前重新查找。
7. **记录结果**：写进卡片文档的「实测结果」小节。脚本自己写错导致的失败要如实说明（例如写死的数量与账号文件不符）。
8. **清理**（见安全规则 5）。

## 常用验证点

用管理面命令行最省事（`UAIG_ADMIN_URL`、`UAIG_ADMIN_TOKEN` 设好后）：

- 计费：`gateway admin call key_stats --args '{"pool":"<池>"}'`、`gateway admin call usage`。
- Key 状态：`gateway admin call pools`（可用数、冷却、用尽、预计用完时间）、`gateway admin call list_keys`。
- 告警与重载：`gateway admin call alerts`、`gateway admin call reload_records`。
- 日志：`gw.boot.log` 里没有 Key / Token 明文（引导 Token 那一行除外）。
