# T60 MinerU 本地文件上传（M3）

- **类型**：规格 + 实现
- **前置**：T59（`DONE`）。不改内核（`internal/core`）与契约（`internal/protocol`），不改分层规则与 adaptertest 套件，不加依赖。
- **授权**：2026-11-12 用户要求「需要支持本地文件上传」。

## 1. 设计

MinerU v4 的本地文件流程：申请上传地址（`POST /api/v4/file-urls/batch`，返回 `batch_id` 与签名地址）→ 把文件 `PUT` 到签名地址 → 上游自动提交解析 → 按批次查询结果（`GET /api/v4/extract-results/batch/{batch_id}`）。

现有路由的请求体是缓冲的 JSON（T59，便于换 Key 重试），文件不能放进去，因此新增两条流式路由（规则见 spec/08 §8.2.1 MU-07、MU-08）：

| 路由 | Op | 行为 |
|:--|:--|:--|
| `POST /v1/parse/file?name=<文件名>` | `parse.file` | 上传后同步等待，返回 Markdown（同 `/v1/parse`） |
| `POST /v1/tasks/file?name=<文件名>` | `task.upload` | 上传后返回任务 ID `batch-<batch_id>`，并固定到上传用的 Key |

- 请求体就是文件本身（`curl --data-binary @file`），必须带 `Content-Length`：签名地址只接受定长上传。解析选项放在查询串里。
- `GET /v1/tasks/{id}` 与 `/result` 识别 `batch-` 前缀，查询批次结果，并转换成与单任务相同的形状，客户端不需要区分两种任务。
- 文件以流的形式直接转发到签名地址，不进内存（EG-07），只尝试一次（CORE-07）。
- 签名地址在第三方存储上：上传时不带上游 Key，也不设 `Content-Type`。地址检查与结果包相同：必须是 https，或与 `base_url` 同源。
- 计费：PUT 成功之前的任何失败都不计费（上游还没有任务）；之后按 T59 的提交后规则处理（不重试，保留计量）。

## 2. 改动文件

- `internal/adapter/mineru/register.go`：`OpParseFile`、`OpTaskUpload`。
- `internal/adapter/mineru/ingress.go`：两条路由（`BodyStream`，200 MiB）；校验 `Content-Length`，读取查询串里的选项。
- `internal/adapter/mineru/egress.go`：`uploadBody`、`validFileName`、`upload`、`doParseFile`、`doTaskUpload`、`getBatch`、`allowedURL`；`doParse` 拆出共用的 `finishParse`；`getTask` 识别 `batch-` 前缀。
- 测试：`upload_test.go`（新增）；`conformance_test.go`（两个入站样例、两个出站样例、3 个非法请求）；`ingress_test.go`（6 条路由）。
- `spec/08_adapters.md`（§8.2 路由表、MU-02、MU-07、MU-08）、`spec/11_tasks.md`（T60）。
- `docs/DEPLOYMENT.md`（新增第 10 节「MinerU 文档解析」，更新反向代理一行）、`deploy/Caddyfile.example`、`deploy/nginx.conf.example`（上传路径改为 `/v1/(parse|tasks)/file`）。

## 3. 测试

- 文件名校验（12 种）；查询参数到请求体的映射（全部选项、最小请求、5 种非法请求）。
- 一致性套件：同步上传（申请地址 → PUT 内容与长度 → 批次结果先为空、再运行中、后完成 → 结果包 → 按 4 页计费）；异步上传返回 `batch-b1` 并固定。
- 第三方存储（TLS 测试服务器）收到的上传不带 Key、不带 `Content-Type`，长度与内容正确。
- 上传完成之前的 5 种失败（Token 错误、日额度、缺少 `batch_id`、非 https 的外部地址、存储拒绝）都不计费，前 4 种不会发出 PUT。
- 批次查询：空列表 → `waiting-file`；运行中带进度与 `data_id`；未完成时下载结果为 409；同步上传后批次失败 → 400 带原因，并保留计量。

## 4. 验收

- [x] spec 已更新；spec/11 中 T60 为 `DONE`。
- [x] `go run ./tools/check` 全绿。
- [x] 实测（2026-11-12，13/13 通过，87 把真实 Key，用本地生成的 2 页测试 PDF，没有上传用户的文件）：同步上传 24 秒返回正确的 Markdown（两页内容都在）；中文文件名加选项（`is_ocr`、`language`、`model_version`）12 秒；异步上传返回 `batch-…`，查询一直落在上传用的 Key 上（waiting-file → pending → running → done），结果包含 `full.md`；缺少 `name`、文件名没有扩展名、分块上传都返回 400；把文件发到 `/v1/parse` 时提示改用 `/v1/parse/file`；计费准确（两次同步各 2 页，加上异步 `?pages=2`，共 6 页）；87 把 Key 全部保持可用；网关日志中没有 Key、账号信息或签名上传地址。

## 5. 已知限制

- 申请上传地址时 Key 失效不会换 Key 重试（流式请求体只有一次尝试），客户端重试即可，调度器已把那把 Key 冷却或隔离。
- 不支持分块上传；一次只上传一个文件（上游支持一批最多 50 个，网关没有暴露）。
- MCP 工具 `mineru_parse` 仍然只接受 URL：网关读不到 AI 客户端所在机器上的文件。
