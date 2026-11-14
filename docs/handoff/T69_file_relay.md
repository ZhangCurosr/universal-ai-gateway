# T69 文件中转（M4，已批准，已完成）

- **类型**：规格 + 实现。2026-11-14 用户批准 §8 的全部建议（「全部按建议」）。
- **前置**：T67（`DONE`）。总览见 [M4_generalization.md](./M4_generalization.md)。
- **约束**：不改 `internal/core` 与 `internal/protocol` 的导出 API；不改分层规则与 adaptertest 套件；不加依赖；不用 CGO。没有开启 `files` 时，行为与现在完全相同。

## 1. 要解决的问题

AI 通过 MCP 调用工具时，参数只能是 JSON。T67 之后 MinerU 的本地文件解析（`POST /parse/file`，请求体是文件本身）已经能用配置表达，但 AI 调不了：它没法把本机的 PDF 放进 JSON 参数里，只能给 URL，而本地文件没有 URL。

本卡让文件「上传一次、到处引用」：

1. 客户端把文件传给网关，得到文件 ID（`file_…`）。
2. 任何声明了文件参数的工具，或者任何 `/rest/` 请求，都可以用文件 ID 代替请求体。网关从本地读出文件，作为请求体发给上游。
3. 文件有大小、数量与保存时间上限，到期自动删除。

AI 本机上运行的 `gateway mcp`（stdio 桥接）能读取本地文件，所以由它提供上传工具：AI 只需要给出文件路径。

## 2. 配置

```yaml
files:
  enabled: true                      # 缺省 false：不注册文件路由，桥接也不提供上传工具
  dir: ""                            # 缺省 <server.data_dir>/files
  max_file_bytes: 209715200          # 单个文件上限（字节，200 MiB）；不能超过 limits.max_stream_body_bytes
  max_files_per_client: 100          # 每个客户端同时保存的文件数
  max_bytes_per_client: 2147483648   # 每个客户端同时保存的总字节数（2 GiB），≥ max_file_bytes
  max_total_bytes: 10737418240       # 全网关总字节数（10 GiB），≥ max_bytes_per_client
  default_ttl: 24h                   # 上传时不指定 ttl 时的保存时间，≥ 1m
  max_ttl: 168h                      # ttl 上限（7 天），≥ default_ttl

models:
  - name: mineru-api
    tools:
      - name: mineru_parse_file
        description: "解析一个已上传的 PDF / 图片 / Office 文件，返回 Markdown。先用 gateway_upload_file 上传得到 file_id"
        method: POST
        path: /parse/file              # T67 的上传类 flow 端点
        file: { arg: file, name_query: name }   # 新增：参数 file 是文件 ID，作为请求体；文件名填到查询参数 name
        input_schema:
          type: object
          properties:
            file:     { type: string, description: "gateway_upload_file 返回的 file_id" }
            is_ocr:   { type: boolean }
            language: { type: string }
          required: [file]
```

## 3. 规则

### 3.1 存储（新包 `internal/files`）

| 编号 | 规则 |
|:--|:--|
| FS-01 | 每个文件在 `dir` 下有两个文件：`<id>.bin`（内容）与 `<id>.json`（元数据 `{id, owner, name, size, sha256, content_type, created_at, expires_at}`）。写入时先写 `<id>.tmp`，边写边算 SHA-256，长度正确后 `rename`；元数据最后写。启动时扫描目录重建内存索引：没有元数据或元数据损坏的 `.bin`、所有 `.tmp` 一律删除，记一条 Warn（只写文件 ID） |
| FS-02 | 文件 ID = `file_` + 26 个 base32 小写字符（`crypto/rand`，128 位）。ID 不是权限凭证：只有上传它的客户端（`owner` = 客户端 ID）能查看、引用、删除；其他客户端得到与不存在时相同的 404 |
| FS-03 | 上限：`max_file_bytes`、`max_files_per_client`、`max_bytes_per_client`、`max_total_bytes`。超过单个文件上限 → 413；超过数量或总量 → 409，消息指明哪一项（先删除旧文件或等它过期）。配额在开始写入前按 `Content-Length` 预占，失败或中断时释放 |
| FS-04 | 到期：`ttl` 取 `default_ttl`，可以在上传时指定（Go 时长，≤ `max_ttl`，否则 400）。后台任务每分钟删除到期文件；正在被请求读取的文件等读取结束后再删（引用计数）。到期后引用 → 404，消息 `file expired or not found` |
| FS-05 | 文件内容不计费、不占客户端的金额额度；上传与删除请求计入客户端的 `rpm_limit`。日志与审计只写客户端 ID、文件 ID、大小与结果，**不写文件名与内容**（文件名常含个人信息） |

### 3.2 数据面 API

| 编号 | 规则 |
|:--|:--|
| FS-10 | `POST /files?name=<文件名>&ttl=<时长>`：请求体是文件本身，必须带 `Content-Length`（> 0），否则 400；分块上传 → 400。`name` 必填，取 base name，≤ 255 字节，不能含控制字符。`Content-Type` 原样记下（缺省 `application/octet-stream`）。读取时限放宽到 `server.read_timeout` + 每 MiB 10 秒（约 100 KiB/s 的最低速度）。成功 201：`{"id", "name", "size", "sha256", "content_type", "created_at", "expires_at"}` |
| FS-11 | `GET /files`：本客户端的文件列表（按创建时间倒序）与用量 `{"files": [...], "used": {"files", "bytes"}, "limits": {...}}`；`GET /files/{id}`：单个文件的元数据；`DELETE /files/{id}`：删除，204。不提供下载内容的接口 |
| FS-12 | 只在 `files.enabled` 时注册。路由经过与协议路由相同的中间件（IP 限流、过载保护、鉴权、客户端限流），错误格式用兜底协议（与 RTE-04 的 404 相同，当前为 OpenAI 风格） |

### 3.3 用文件 ID 代替请求体

| 编号 | 规则 |
|:--|:--|
| FS-20 | 请求头 `X-UAIG-File: <id>`：网关在鉴权之后、交给适配器之前，把请求体替换为该文件的内容，`Content-Length` 设为文件大小；客户端自带的请求体必须为空（否则 400）。只对请求体为 `BodyStream` 或 `BodyBuffered` 的路由生效（缓冲路由仍受该路由的 `MaxBodyBytes` 限制，超过 → 413）；`BodyNone` 路由带这个头 → 400。文件不存在、已过期或不属于本客户端 → 404 |
| FS-21 | `X-UAIG-File-Name-Query: <参数名>`（与 FS-20 一起用）：查询串里没有该参数时，把文件名作为它的值追加进去；参数名只允许 `[A-Za-z0-9_.-]` |
| FS-22 | 替换在协议无关的位置完成：直接 HTTP 请求在 `internal/httpapi` 的中间件里；工具调用在 `internal/mcp` 构造内部请求时（工具调用不经过 HTTP 中间件）。两处都在替换后删除这两个头，适配器看不到它们 |
| FS-23 | 文件作为请求体时，计费、选 Key、重试与直接上传完全相同：`BodyStream` 路由只尝试一次（CORE-07），`BodyBuffered` 路由可以换 Key 重试（读入内存的是文件内容） |
| FS-24 | 过载保护（OVL-02）在鉴权之前执行，那时还不知道文件大小：带 `X-UAIG-File` 的请求按未知长度占用请求体预算（单个请求的上限）。工具调用中的文件请求体不另占预算（与工具调用的其他请求体相同） |

### 3.4 工具声明中的文件参数（`rest` 适配器）

| 编号 | 规则 |
|:--|:--|
| FS-30 | `models[].tools[].file: {arg, name_query?}`：只允许 `POST` / `PUT`（配置校验）。调用时参数 `arg` 必须是字符串（文件 ID），移出参数表，放进 FS-20 的请求头；`name_query` 放进 FS-21 的请求头；其余参数按 GET 的规则放进查询串（RS-42 的标量规则），请求体为空 |
| FS-31 | `files.enabled` 为 false 而配置了 `file` 参数 → 网关拒绝启动，消息指明工具名 |

### 3.5 上传入口

| 编号 | 规则 |
|:--|:--|
| FS-40 | `gateway mcp`（stdio 桥接）加 `--upload-dir <目录>`（可重复）后，在 `tools/list` 的结果末尾追加本地工具 `gateway_upload_file {path, ttl?}`（非只读），由桥接自己执行，不转发给网关：`path` 解析为绝对路径并解开符号链接后必须位于某个 `--upload-dir` 之内，否则 `isError`；然后按 FS-10 上传，结果文本是 FS-10 的 JSON。没有 `--upload-dir` 时不提供这个工具（AI 不能借它读取任意文件） |
| FS-41 | 命令行：`gateway files upload <路径> [--ttl 2h]`、`gateway files list [--json]`、`gateway files delete <id>`，用 `UAIG_URL` / `UAIG_TOKEN`（与 `gateway call` 相同） |
| FS-42 | 远程 MCP（不经过桥接）的 AI 只能用 FS-41 或 FS-10 上传，本卡不提供 base64 上传工具 |

### 3.6 管理面

| 编号 | 规则 |
|:--|:--|
| FS-50 | `GET /admin/api/files`：`{"enabled", "used": {files, bytes}, "clients": [{client_id, files, bytes}], "expiring": [{id, client_id, size, expires_at}], "limits"}`；`expiring` 是最近到期的 20 个文件，不含文件名。没有开启时只有 `{"enabled": false}`。`DELETE /admin/api/files/{id}`：管理员删除任意文件 |
| FS-51 | 概览页的指标行增加「文件中转」：`已用字节 / max_total_bytes` 与文件数，超过 80% 黄色；`files.enabled` 为 false 时不显示 |

## 4. 实现要点

- `internal/files`：`Store{Put, Get(owner, id) (io.ReadCloser, Meta), List, Delete, Sweep}`，内存索引加锁，磁盘写入在请求 goroutine 中完成，不经过数据库。
- `internal/httpapi`：文件路由与 FS-20 中间件；`internal/mcp`：`Deps.Files`，`newInternalRequest` 中执行 FS-20 / FS-21；桥接中的本地工具在 `mcp.Bridge` 里拦截 `tools/list` 与 `tools/call`。
- 配置：`FilesConfig`（CFG-23），大小用已有的字节数解析；`files` 段改动需要重启（不进 `hotSections`）。
- 装配：`serve.go` 创建 Store、启动清理任务（worker `files_sweep`）、把 Store 交给 httpapi 与 mcp。
- 架构说明：spec/01 §4 规定数据面热路径不做磁盘 I/O。文件中转本身就是读写磁盘，只发生在文件路由与带 `X-UAIG-File` 的请求上，普通模型调用不受影响。这是一条明确的例外，需要写进 spec/01（见决策 1）。

## 5. 测试

- Store：写入与校验和、中断写入释放配额、启动时清理残留、到期删除、引用计数延迟删除、四种上限、跨客户端 404。
- 数据面 API：缺少长度、分块、超过上限、`name` 非法、`ttl` 超过上限、列表与删除。
- 替换：直接 HTTP（流式路由、缓冲路由、超过路由上限、`BodyNone` 路由、自带请求体）；工具调用（`file` 参数、`name_query`、文件不属于本客户端）；适配器收不到两个内部头。
- 桥接：`--upload-dir` 之外的路径、符号链接逃逸、不带 `--upload-dir` 时没有该工具、其他消息照常转发。
- 端到端（假上游）：上传 → 工具 `mineru_parse_file` 用文件 ID 调用 T67 的上传 flow，假存储收到的字节与原文件相同，计费与直接上传相同。

## 6. 实测（验收）

用真实 MinerU Key，只上传本地生成的测试 PDF，不上传用户的文件：

1. 通过 `gateway mcp --upload-dir <临时目录>` 调用 `gateway_upload_file`，再调用 `mineru_parse_file`，一次得到 Markdown，计费页数正确。
2. 同一个文件 ID 再解析一次，不需要重新上传；另一个客户端用这个 ID → 404。
3. `ttl: 1m` 的文件到期后被删除，引用返回 404，磁盘上没有残留。
4. 上传目录以外的路径被拒绝；日志与审计中没有文件名、Key 或签名上传地址。

### 6.1 实测结果（2026-11-14，通过）

用真实 MinerU Key（账号文件当时有 88 把有效 Key），只上传本地生成的 2 页测试 PDF（文件名含中文与空格），没有上传用户的文件。

- [x] `gateway mcp --upload-dir <临时目录>` 的 `tools/list` 中有 `gateway_upload_file` 与 `mineru_parse_file`；上传得到 `file_…`，大小与原文件一致。
- [x] `mineru_parse_file {"file": id, "language": "en"}` 一次返回两页内容的 Markdown（20 秒）；同一个 ID 用 `X-UAIG-File` 直接 HTTP 再解析一次（9 秒），不需要重新上传；共计 4 页，与两次解析一致。
- [x] 另一个客户端用这个 ID：直接请求 404，工具调用返回 `file expired or not found`。上传目录以外的路径被拒绝。
- [x] `ttl: 1m` 的文件在约 2 分钟内被删除：查询与引用返回 404，磁盘上只剩另一个文件的两项。管理视图显示 2 个文件、不含文件名。
- [x] 88 把 Key 全部保持可用；网关日志与桥接 stderr 中没有 Key、Token 或文件名。
- 脚本中有两项写死了 87 把 Key，因账号文件新增了一行而报告失败；实际行为正确（导入 88、可用 88）。

## 7. 改动文件

- 新增 `internal/files`（Store 与测试）。
- `internal/config`：`FilesConfig`、`ToolConfig.File` 与校验（CFG-23）。
- `internal/httpapi`：文件路由、FS-20 中间件（`router.go`、新文件 `files.go`）。
- `internal/mcp`：`invoke.go`（FS-20 / FS-21）、`client.go`（桥接本地工具）。
- `internal/adapter/rest`：`tools.go`（FS-30）。
- `internal/admin`：`files.go`（FS-50）、概览页卡片。
- `cmd/gateway`：`serve.go`（装配）、`client_cmds.go`（`files` 子命令、`mcp --upload-dir`）、`rest_api.go`（转换）、端到端测试。
- `internal/archtest`：新包必须登记分层（未登记的包测试直接失败，「新包需要架构批准」）。只加一行 `"internal/files": 2`，不改任何规则，见决策 8。
- 规格：spec/01 §4（例外）、spec/03 CFG-23、spec/08 §8.3.5（FS-30）、spec/09（FS-10 ~ FS-12、FS-50 / FS-51）、spec/14（FS-40 ~ FS-42）、spec/11 T69；`docs/DEPLOYMENT.md`、`gateway.example.yaml`、`.claude/skills/add-api/SKILL.md`。

## 8. 待决策

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 数据面热路径不做磁盘 I/O（spec/01 §4） | A. 在 spec/01 写明例外：只限文件路由与带 `X-UAIG-File` 的请求；B. 文件放内存 | **A**：200 MiB 的 PDF 放内存不现实；例外范围清楚，普通调用不受影响 |
| 2 | 元数据存在哪里 | A. 每个文件旁边一个 JSON，启动时扫描重建索引；B. SQLite 新表 | **A**：数据面 handler 不访问数据库（AGENTS.md §3 第 2 条）；文件与元数据放在一起，删目录就干净 |
| 3 | AI 怎么上传本地文件 | A. 桥接的本地工具，必须用 `--upload-dir` 指定允许的目录；B. 桥接允许任意路径；C. 只提供命令行 | **A**：AI 只给路径就能用；限制目录避免借工具读取 Key 文件之类的敏感文件 |
| 4 | 工具怎么引用文件 | A. 工具声明 `file: {arg, name_query}`，网关替换请求体；B. 让 AI 自己拼 `X-UAIG-File` 头 | **A**：AI 只看到一个 `file` 参数；B 需要工具能设请求头，MCP 参数里没有这个概念 |
| 5 | 是否提供下载文件内容的接口 | A. 不提供；B. 提供 `GET /files/{id}/content` | **A**：本卡只做「交给上游」，少一个泄露面；以后需要再加 |
| 6 | 是否提供带签名的公开下载地址（给只收 URL 的上游，如 MinerU 在线解析） | A. 不提供；B. 提供（`?sig=`，需要网关可从公网访问） | **A**：多数部署在内网或本机，上游访问不到；MinerU 已有上传 flow |
| 7 | 远程 MCP 是否提供 base64 上传工具 | A. 不提供；B. 提供，受 MCP 消息大小限制 | **A**：base64 让文件内容进入 AI 上下文，既费 token 又不适合大文件 |
| 8 | 文件存储放在哪个包 | A. 新包 `internal/files`，在 archtest 中登记为第 2 层（只依赖 `config`、`obs`），这是 AGENTS.md §3 第 10 条要求批准的改动；B. 放进 `internal/httpapi`，由 `mcp` 通过接口注入 | **A**：`httpapi` 与 `mcp` 同在第 4 层，都要用 Store；只加登记、不改规则，分层关系清楚 |
