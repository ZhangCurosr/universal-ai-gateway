# T67 声明式 API 能力 B：同步等待与预签名上传（M4，规格草案）

- **类型**：规格 + 实现。2026-11-13 用户批准 §7 的全部建议（「全部按建议」）。
- **前置**：T66（`DONE`）。总览见 [M4_generalization.md](./M4_generalization.md)。
- **约束**：只改 `rest` 适配器、配置与装配代码。不改内核与契约的导出 API；不改分层规则与 adaptertest 套件；不加依赖。没有 `flow` 的端点行为与 T66 完全相同。

## 1. 要解决的问题

T66 之后，通用转发已经能处理业务错误码、按字段计费、异步任务固定，但还有两种很常见的模式只能写专用适配器：

| 模式 | 例子 | 为什么 T66 不够 |
|:--|:--|:--|
| **同步等待**：一次请求内「提交 → 轮询 → 取结果」 | MinerU `/v1/parse`：提交解析，等完成，下载结果包取出 `full.md` 返回；AI 工具只想要一次调用拿到 Markdown | T66 只转发单个请求，客户端要自己轮询；异步任务只能在提交时按估算计费 |
| **预签名上传**：先申请上传地址，再把文件 `PUT` 过去 | MinerU 本地文件、各家对象存储式上传 | 需要两步请求，第二步发往第三方存储，且不能带上游 Key |

视频生成、语音转写、OCR、批量翻译都是同样的形态。

## 2. 配置

在 T66 的 `endpoints` 里，端点可以写 `flow`：这时 `match` 是**网关上的虚拟路径**，不转发到上游同名路径，而是执行 `flow` 中的几步上游请求。

```yaml
models:
  - name: mineru-api
    pool: mineru-rest                  # cooldown: { max: 24h }; timeouts: { response_header_base: 10m }
    price: { pages: "0.01", requests: "0" }
    reserve: { pages: 600 }
    api:
      errors: [ ... ]                  # 与 T66 相同，对 flow 中的每一步上游请求都生效
      endpoints:
        # 在线文件，同步：POST /rest-idem/mineru-api/parse，请求体 {"url": "..."}，直接返回 Markdown
        - match: "POST /parse"
          flow:
            submit:
              request: "POST /api/v4/extract/task"   # 请求体原样转发
              task_id: data.task_id
            poll:
              request: "GET /api/v4/extract/task/{task_id}"
              state: data.state
              done: [done]
              failed: [failed]                       # 失败：400，带上游响应体
            result:
              url: data.full_zip_url                 # 从完成时的轮询响应里取下载地址
              zip_entry: full.md                     # 取结果包里名为 full.md 的文件（可在子目录）
              content_type: "text/markdown; charset=utf-8"
          usage: { pages: { json: data.extract_progress.total_pages, zip_json: { entry: layout.json, count: pdf_info } } }   # 取轮询中最后一次见到的值

        # 本地文件，同步：POST /rest/mineru-api/parse/file?name=a.pdf&is_ocr=true，请求体是文件本身
        - match: "POST /parse/file"
          flow:
            upload:
              request: "POST /api/v4/file-urls/batch"
              body:                                  # 申请上传地址的请求体，由查询参数组成
                files.0.name: { query: name, required: true }
                files.0.is_ocr: { query: is_ocr, type: bool }
                files.0.page_ranges: { query: page_ranges }
                enable_formula: { query: enable_formula, type: bool }
                language: { query: language }
                extra_formats: { query: extra_formats, type: list }
              upload_url: data.file_urls.0           # 把请求体 PUT 到这个地址
              task_id: data.batch_id
            poll:
              request: "GET /api/v4/extract-results/batch/{task_id}"
              state: data.extract_result.0.state     # 取不到（刚上传）视为仍在进行
              done: [done]
              failed: [failed]
            result:
              url: data.extract_result.0.full_zip_url
              zip_entry: full.md
              content_type: "text/markdown; charset=utf-8"
          usage: { pages: { json: data.extract_result.0.extract_progress.total_pages, zip_json: { entry: layout.json, count: pdf_info } } }

        # 本地文件，异步：只上传，返回任务 ID 并固定到上传用的 Key；之后用 T66 的 pinned_by 查询
        - match: "POST /tasks/file"
          flow:
            upload: { request: "POST /api/v4/file-urls/batch", body: { files.0.name: { query: name, required: true } }, upload_url: data.file_urls.0, task_id: data.batch_id }
          usage: { pages: { request_query: pages, default: 600 } }
        - match: "GET /api/v4/extract-results/batch/{batch}"
          pinned_by: batch
          usage: {}
```

### 2.1 规则

| 编号 | 规则 |
|:--|:--|
| FL-01 | 有 `flow` 的端点是虚拟路径：客户端请求 `/rest/{service}/<match 路径>` 或 `/rest-idem/{service}/<match 路径>` 时执行 flow，不再转发到上游的同名路径。flow 由 `submit` 或 `upload`（二选一，必填一个）、可选的 `poll`、可选的 `result` 组成：只有第一步时就是「提交后立即返回」，有 `poll` 时是同步等待 |
| FL-02 | `submit`：按 `request`（方法 + 上游路径）发送客户端请求体（只能用于缓冲请求体的路由，即 `/rest-idem/`，否则配置校验失败）。`task_id` 取任务 ID。客户端查询串原样转发 |
| FL-03 | `upload`：① 按 `request` 发送由 `body` 组成的 JSON（见 FL-04）；② 从响应的 `upload_url` 字段取上传地址，把客户端请求体 `PUT` 过去：定长（客户端必须带 `Content-Length`，> 0，否则 400）、不设 `Content-Type`、流式转发不进内存；③ `task_id` 取任务 ID。只能用于流式请求体的路由（`/rest/` 下的 `POST`）。上传地址必须是 `https`，或与 `base_url` 同源，否则 502；**只有同源时才带上游 Key**，第三方存储不带 Key。地址只在内存中使用，不写日志 |
| FL-04 | `body` 是「目标字段路径 → 来源」的映射。来源只有 `query`（查询参数名），`type` 取 `string`（缺省）、`bool`（`strconv.ParseBool`）、`int`、`list`（逗号分隔的字符串数组）；值不合法 → 400 并指明参数名；查询参数缺失时省略该字段，`required: true` 时缺失 → 400。目标路径中的数字段创建数组，只允许从 0 开始依次出现（`files.0.name`） |
| FL-05 | `poll`：用同一把 Key（EG-05）按 `request` 查询，`{task_id}` 替换为任务 ID（按单个路径段转义）。`state` 字段的值在 `done` 中 → 完成；在 `failed` 中 → 失败（400，`client_error`，带该次查询的响应体，截断到 8 KiB）；其他值或取不到 → 继续。间隔从 2 秒开始，每次乘 1.5，最长 15 秒；总等待受请求上下文与 `timeout`（缺省 10 分钟，上限 1 小时）限制，超时 → 504 |
| FL-06 | `result`：没有时返回完成时的那次查询响应（JSON 原样）。有 `url` 时从完成响应取下载地址并下载（地址规则同 FL-03，只有同源才带 Key）；有 `zip_entry` 时把下载内容当作 zip，返回名字等于 `zip_entry` 的文件（可以在子目录中，取第一个匹配项），`content_type` 为响应的 `Content-Type`（缺省 `application/octet-stream`）。没有 `zip_entry` 时原样流式转发下载内容 |
| FL-07 | 大小限制（防压缩炸弹）：zip 下载与解出的单个文件都以 `MaxBufferedBytes`（64 MiB）为上限，超过 → 502，不惩罚 Key。zip 中找不到 `zip_entry` → 502 |
| FL-08 | 错误与重试：T66 的错误规则对每一步上游请求都生效（含 2xx 业务错误）。第一步（`submit` 的提交、`upload` 的申请地址）成功之前的失败：按结果分类换 Key（只在缓冲请求体的路由上，DA-12；上传是流式请求体，只尝试一次）。**第一步成功之后**（任务已在上游创建）：不换 Key、不重试，失败时按 CORE-12 同时返回计量结果与错误，保留计费 |
| FL-09 | 计费：端点的 `usage`（规则同 DA-07）在 flow 中这样求值：`json` 依次对每次轮询响应求值，**取最后一次取到的值**（完成时的值优先）；`request_json` / `request_query` / `default` 同 DA-07。没有 `poll` 的 flow（提交后立即返回）只对第一步的响应求值。全部取不到时 `UsageNone`（按预留结算） |
| FL-10 | 固定：flow 取到任务 ID 时放进 `Response.Pins`（值同 DA-08），之后可以用 T66 的 `pinned_by` 端点查询同一任务 |
| FL-11 | 响应头：同步结果按 FL-06 设置 `Content-Type`；返回 zip 文件内容时不转发上游的 `Content-Length`、`Content-Disposition`。日志只写服务名、步骤名、状态码与耗时，不写任务 ID 以外的字段值、请求体、上传地址或结果内容 |

## 3. 实现要点

- 配置：`APIEndpoint` 增加 `Flow *APIFlow`（`submit`、`upload`、`poll`、`result`）；静态校验：`submit` 与 `upload` 恰好一个；`request` 的方法与路径语法；`poll.request` 必须含 `{task_id}`；`done` 非空；`body` 目标路径的数组下标从 0 依次出现、`type` 合法；有 `flow` 时 `pinned_by`、`task_id`（端点级）不能同时出现；`timeout` ≤ 1 小时。
- 适配器：新增 `flow.go`，执行 FL-01 ~ FL-11；复用 T66 的字段路径、错误规则、用量计算。zip 读取用标准库 `archive/zip`（不是新依赖）。
- 路由：不新增路由。flow 端点在 `/rest/`、`/rest-idem/` 下按 `match` 命中；上传类 flow 的 `Content-Length` 检查放在入站 `Parse`（与 MU-07 相同）。
- 超时：同步等待会在发出响应头之前等很久，池需要配置 `timeouts.response_header_base`（例如 `10m`），部署文档写明。
- 契约：只用已有机制（`BodyStream`、`AttemptError`、`Response` 与 error 同时返回、`Pins`、`Meter`）。如果发现不够用，停下来报告。

## 4. 测试

- 配置校验：每条静态规则的正反例。
- `body` 组装：四种类型、缺失、`required`、非法值、数组下标。
- 同步提交：提交 → 轮询（进行中、完成、失败、取不到状态、超时）→ 结果（原样 JSON、下载、zip 取文件、zip 中没有、超过上限、非 https 第三方地址）。
- 上传：申请地址 → `PUT`（第三方存储不带 Key、同源带 Key、不设 `Content-Type`、长度正确、存储拒绝）→ 轮询 → 结果；缺少 `Content-Length`、分块上传 → 400。
- 错误与计费：第一步之前的失败换 Key 且不计费；之后的失败不重试、保留计量；页数取最后一次见到的值。
- 端到端（假上游）：用配置重写 MinerU 的三个用法，与专用适配器的一致性测试场景逐一对照。

## 5. 实测（验收）

用真实 Key，只用配置，与专用适配器 `mineru` 对照：

1. 在线文件同步解析（demo PDF）：返回的 Markdown 与 `mineru` 适配器相同，计费页数相同。
2. 本地文件同步解析（本地生成的 2 页 PDF，不上传用户的文件）：Markdown 包含两页内容，计 2 页；中文文件名加选项。
3. 本地文件异步：返回任务 ID，用 `pinned_by` 查询直到完成，查询始终落在上传用的 Key 上。
4. 缺少 `name`、分块上传返回 400；87 把 Key 全部保持可用；日志中没有 Key、账号信息或签名上传地址。

### 5.1 实测结果（2026-11-13，17/17 通过）

只用配置（通用 `rest` 适配器），与专用 `mineru` 适配器在同一个网关中对照，各 87 把真实 Key；本地文件用本地生成的 2 页测试 PDF，没有上传用户的文件。

- [x] 在线文件同步解析（demo PDF）：一次请求返回 Markdown，与 `mineru` 适配器**逐字节相同**（51684 字节），都按 13 页计费；耗时 23 秒（适配器 21 秒）。
- [x] 本地文件同步上传：Markdown 含两页内容；中文文件名加 `is_ocr`、`language`、`model_version` 正常。
- [x] 本地文件异步上传：只返回 `{"task_id"}`（不含签名上传地址）；之后的查询一直落在上传用的 Key 上（waiting-file → pending → done）。
- [x] 缺少 `name`、分块上传、在 `/rest/` 下调用提交 JSON 的 flow 都返回 400，消息指明原因。
- [x] 计费：flow 共 19 页（13 + 2 + 2 + 异步 `?pages=2`），与适配器一致；174 把 Key 全部保持可用；导入输出与网关日志中没有 Key、账号信息或签名上传地址。
- 首次实测（15/17）发现小文件直接从 `pending` 到 `done`，轮询中从未出现页数，2 页文件按预留结算了 600 页；用户同意后新增 `zip_json` 来源（FL-09a），第二次实测计费准确。

## 6. 改动文件

- `internal/config/api.go`（flow 与 `zip_json` 的类型、校验）及测试。
- `internal/adapter/rest`：新增 `flow.go`、`flow_test.go`；`api.go`（端点增加 flow、`zip_json`、`permission_denied` 在 2xx 上同样按 RS-16 处理）、`ingress.go`（上传长度检查）、`egress.go`（分派到 flow；错误分类抽成 `classifyError` 供各步复用）。
- `cmd/gateway/rest_api.go`（转换）、`e2e_rest_api_test.go`（新增 `TestE2E_REST_DeclaredFlow`）。
- `spec/03` CFG-20、`spec/08` §8.3.7（新增 FL 规则）、`docs/DEPLOYMENT.md` §9.8、`.claude/skills/add-api/SKILL.md`、`gateway.example.yaml`。

## 7. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | flow 放在哪里 | A. 端点上写 `flow`，`match` 是网关上的虚拟路径；B. 另起一个顶层 `flows` 列表 | **A**：与 T66 的端点、计费、工具声明放在一处；工具可以直接指向虚拟路径，AI 一次调用拿到结果 |
| 2 | 申请上传地址的请求体怎么组成 | A. 「目标字段 → 查询参数」的映射，带四种类型；B. 字符串模板（如 `"{\"name\":\"{{query.name}}\"}"`） | **A**：不会拼出非法 JSON，也不会注入；B 实际上是一门模板语言 |
| 3 | 是否支持从 zip 里取文件 | A. 支持（`zip_entry`，按文件名匹配，有大小上限）；B. 不支持，只原样转发下载内容 | **A**：文档、OCR 类 API 常把结果打包成 zip；不支持的话 MinerU 的同步解析就无法用配置表达 |
| 4 | 轮询参数 | A. 固定 2 秒起、×1.5、最长 15 秒，只开放 `timeout`；B. 间隔也可配置 | **A**：少一个容易配错的旋钮；有 API 要求更慢的轮询时再加 |
| 5 | 与专用适配器的关系 | A. T67 完成后保留 `mineru` 适配器；B. 改为配置并删除 `mineru` 适配器 | **A**：先并行对照一段时间，再单独立卡决定是否删除 |

## 7.1 实现时的补充（已写入 spec/08 §8.3.7）

- 没有 `poll` 的上传 flow 只返回 `{"task_id": ...}`：申请地址的响应含签名上传地址，不交给客户端。
- 第一步成功之后，可重试的失败改写为 `local_error`（不重试、不惩罚 Key），与 `mineru` 适配器的 `afterSubmit` 相同。
- 存储拒绝签名上传（401 / 403）、结果地址不可用都按 `server_error` 处理，不当作 Key 的问题；存储的错误体不转发。
- flow 中每一步的响应都必须是 JSON（否则 502），错误规则对每一步都生效。
- 规则把 2xx 响应映射为 `permission_denied` 时，同样按 RS-16 改写为 403 客户端错误（T66 只在非 2xx 上做了这一步，本卡一并修正）。

## 8. 与专用适配器的已知差异

- ~~页数：声明式版本只看轮询响应~~（已解决）：首次实测发现小文件直接从 `pending` 到 `done`，轮询中从未出现 `extract_progress`，2 页文件按预留结算了 600 页。2026-11-13 用户同意新增 `zip_json` 来源（FL-09a），与 `mineru` 适配器一样数 `layout.json` 中 `pdf_info` 的长度。
- 异步上传任务的查询返回上游原始形状（`extract_result` 列表），不像 `mineru` 适配器那样转换成与单任务相同的形状。
- 任务固定记录仍只在内存里（T68 解决）。
