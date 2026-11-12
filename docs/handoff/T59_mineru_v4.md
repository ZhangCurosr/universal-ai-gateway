# T59 MinerU 适配器对接官方精准解析 API v4（M3）

- **类型**：规格 + 实现
- **前置**：T31（`DONE`）。不改内核（`internal/core`）与契约（`internal/protocol`），不改分层规则与 adaptertest 套件，不加依赖（结果包用标准库 `archive/zip` 解压）。
- **授权**：2026-11-12 用户导入 87 把 MinerU Key 实测时发现问题，要求「在这里改」。

## 1. 背景

T31 的 `mineru` 适配器按 spec/08 §8.2 实现，出站调用的是一个通用的 `/v1/tasks` 接口（规格只「以 MinerU 为例」）。MinerU 官方的接口是 `/api/v4/extract/task`，所以这个适配器连不上 mineru.net。

实测时临时改用通用 `rest` 适配器转发 v4：提交 3/3 成功，但查询任务 40 次全部返回 `-60012`（找不到任务）。原因是任务只存在于创建它的账号下，而 `rest` 没有任务固定，查询换了 Key。只留一把 Key 时任务 20 秒完成。`mineru` 适配器本来就有任务固定（`Pins` + `pin_ttl` + 严格亲和），只缺正确的上游协议。

## 2. 改动

客户端路由不变（`POST /v1/parse`、`POST /v1/tasks`、`GET /v1/tasks/{id}`、`GET /v1/tasks/{id}/result`、MCP 工具 `mineru_parse`），出站改为 v4，规则见 spec/08 §8.2.1 MU-01 ~ MU-06：

| 项 | 之前 | 之后 |
|:--|:--|:--|
| 上游接口 | `/v1/tasks`、`/v1/tasks/{id}`、`/v1/tasks/{id}/result`（不存在） | `POST /api/v4/extract/task`、`GET /api/v4/extract/task/{id}`、`data.full_zip_url` 结果包 |
| 请求体 | 流式（文件或 JSON） | 缓冲的 JSON（≤ 64 KiB，`url` 必填），提交前失败可以换 Key 重试；不支持文件上传 |
| 同步封装结果 | 上游结果原样 | 下载结果包，取出 `full.md` |
| 页数计费 | 猜测多个字段 | `extract_progress.total_pages`，没有时取 `layout.json` 的 `pdf_info` 长度，仍没有按预留 |
| 错误 | 只看 HTTP 状态 | 解析业务码：Token 错误 → 401 换 Key；日额度用完 → 429 冷却到北京时间次日；队列满等 → 503；找不到任务 → 404 |
| 结果包下载 | — | 只允许 https 或与 `base_url` 同源；第三方 CDN 不带 Key；解压大小受上限约束 |

配置建议（`gateway.example.yaml` 已更新）：池 `cooldown: { max: 24h }`，让日额度用完的 Key 冷却到次日，而不是每 10 分钟重试一次。

## 3. 测试

- `internal/adapter/mineru`：重写出站测试（假的 v4 上游）。覆盖提交与轮询、`full.md` 提取（含子目录）、两种页数来源、页数未知按预留、业务码分类（10 个码）、日额度冷却时长（含跨年）、提交被拒不计费、提交后失败不重试且保留计量、任务失败、结果包 409/404/下载、第三方 CDN 不带 Key、外部地址被拒、压缩炸弹上限、参数校验（10 种非法请求）。
- 一致性套件（adaptertest，未修改）：出站样例改为 v4 上游，全部通过。
- `internal/core/hardening_test.go` 的 `TestPaidMultiStepFailureSettlesOnce` 用 MinerU 作为真实多步适配器：假上游改为 v4 格式，期望不变（502、3 次上游调用、按 7 页结算一次）。
- 入站路由测试：`parse`、`task.create` 改为 `BodyBuffered` + 64 KiB。

以上测试期望的修改都源于上游协议变更，用户已授权本卡。

## 4. 验收

- [x] spec/08 §8.2 更新并新增 §8.2.1；spec/11 中 T59 为 `DONE`。
- [x] `go run ./tools/check` 全绿。
- [x] 实测（2026-11-12，12/12 通过，87 把真实 Key，`protocol: mineru`，`base_url: https://mineru.net`）：同步 `/v1/parse` 两次各约 33 秒返回 51 KB Markdown；MCP `mineru_parse` 22 秒；异步提交后经网关查询 8 次一直落在创建任务的 Key 上（pending → running → done，之前用 `rest` 时 40 次全部 -60012），未完成时下载结果为 409，完成后下载到 0.99 MB 的结果包；缺少 `url` → 400，未知任务 → 404；同步解析按实际 13 页计费；87 把 Key 全部保持可用；网关日志与导入输出中没有任何 Key 或账号信息。
- 注意：异步提交没有给 `pages` 查询参数时按预留（600 页）结算，这是 §8.2 原有规则；需要精确计费的调用方应使用同步封装或在提交时给出 `?pages=`。

## 5. 已知限制

- 不支持本地文件上传。v4 的上传流程是先申请签名地址（`/api/v4/file-urls/batch`）再 PUT 文件，任务按批次查询，与现有的单任务路由不同，留待后续。
- 异步透传提交时仍按估计页数结算（§8.2 原规则，适配器无状态）。
