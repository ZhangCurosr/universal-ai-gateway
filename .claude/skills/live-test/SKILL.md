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
5. 测完：停掉网关（Windows 用 `taskkill //F //IM gateway.exe`，不加 `/F` 旧进程会留着），删除临时目录里的 `data`、`secrets`、`gw*.log`、生成的 Token 文件。

## 步骤

1. **编译到临时目录**（不要编译到仓库根目录）：
   `go build -o <scratch>/imp/gateway.exe ./cmd/gateway`
2. **写配置**：监听 `127.0.0.1:18496`（数据面）/ `127.0.0.1:18497`（管理面），`data_dir: ./data`，`master_key_file: ./secrets/master.key`。先 `gateway.exe check-config --config x.yaml`。
   已知的坑（`check-config` 查不出来，启动时才报错）：
   - `openai_chat` 协议的服务必须给 `price.cache_read_tokens` 定价；
   - `mineru` 协议会上报 `requests` 单位，必须定价；
   - `rest` 协议有 `api.endpoints[].usage` 时，用到的每个单位都要在 `price` 里。
3. **在后台启动**：`./gateway.exe serve --config x.yaml > gw.boot.log 2>&1`，等 `/healthz` 返回 200。
4. **导入 Key**（输出只有计数）：
   ```bash
   UAIG_ADMIN_URL=http://127.0.0.1:18497 UAIG_ADMIN_TOKEN=<引导 Token> \
     ./gateway.exe keys import --pool <池> --file <账号文件路径> --where status=ok --name-prefix <前缀>
   ```
   CSV 加 `--format csv`；有过期时间加 `--expires-field expires_at --tz +08:00`。
5. **跑实测脚本**：复制 `livetest_template.py`，按卡片的验收步骤写 `check(...)`；每一项打印 `PASS` / `FAIL` 与不含敏感值的细节，最后打印 `N/M passed`。耗时长的放后台运行，输出写文件。
6. **浏览器验证 UI**（需要时）：打开 `http://127.0.0.1:18497/admin/ui/`，用引导 Token 登录；页面每 10 秒刷新，元素引用会失效，点击前重新查找。
7. **记录结果**：写进卡片文档的「实测结果」小节。脚本自己写错导致的失败要如实说明（例如写死的数量与账号文件不符）。
8. **清理**（见安全规则 5）。

## 常用验证点

- 计费：`GET /admin/api/keys/stats?pool=<池>` 的 `usage`、`GET /admin/api/usage`。
- Key 状态：`GET /admin/api/pools`（可用数、冷却、用尽）。
- 日志：`gw.boot.log` 里没有 Key / Token 明文（引导 Token 那一行除外）。
