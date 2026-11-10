# T51 通用 REST 转发适配器（M3）

- **类型**：规格 + 实现
- **前置**：无。只新增一个适配器包，不改内核、契约和转译层，可以和 T50 并行。
- **需要用户审批**：**是**（§3 规格草案，含 4 个待定项）。阶段 A 只提交 spec 修改，提交信息写「待审批」，然后**停下等用户回复**。
- **必读**：
  - [`spec/08_adapters.md`](../../spec/08_adapters.md) §2.1、§3.1（HDR-01 ~ HDR-05）、§5、§6、§7、§8（尤其 §8.3 的设计草案）
  - [`spec/13_core.md`](../../spec/13_core.md) CORE-07（流式请求体只尝试一次）、§4（`ResponseRaw` 的转发）
  - [`spec/07_scheduler.md`](../../spec/07_scheduler.md) §6（RTY-01 ~ RTY-06，重试与换 Key 的条件）
  - [`spec/05_security_auth.md`](../../spec/05_security_auth.md) AUTH-01（客户端凭据从哪些头读取）
  - `internal/protocol/`：`protocol.go`（`Route`、`Envelope`）、`egress.go`、`httpx.go`（`Send`、`JoinURL`、`CopyHeaders`）、`classify.go`
  - `internal/adapter/tavily/`（最接近的范例：无模型字段、按次计费）、`internal/adapter/mineru/`（`BodyStream` 与 `ResponseRaw` 的用法）
  - `internal/core/core.go` 中 `OutcomeClientError` 的处理（同协议透传时原样转发上游错误体）
  - `internal/adapter/adaptertest/`

## 1. 背景与目标

现在每接入一个非大模型 API（搜索、文档解析……）都要写一个适配器包，它们大多只做三件事：**换上游 Key、按次计费、原样转发**。spec/08 §8.3 早就设计过一个通用转发适配器，但一直没实现。

本卡实现它：协议名 `rest`，包 `internal/adapter/rest`。接入一个新的 REST API 只需要在配置里加一个池和一个服务名，**不写代码**：

```yaml
pools:
  - name: weather
    protocol: rest
    base_url: https://api.weather.example/v2
models:
  - name: weather            # 客户端用 /rest/weather/... 访问
    pool: weather
    price: { requests: "0.001" }
    reserve: { requests: 1 }
```

```bash
curl -H "Authorization: Bearer <网关 Token>" \
  "http://gateway:8080/rest/weather/forecast?city=Paris"
# → 网关换上上游 Key，转发 GET https://api.weather.example/v2/forecast?city=Paris
```

代价是没有协议级的用量、错误格式与亲和：只能按次计费，错误原样返回，不做会话亲和。

**明确不做**：

- 按请求体字段或路径计费（需要按池配置的适配器选项，属于修改冻结契约的工作，留给之后的架构决策卡，与 WASM 插件一起考虑）。
- 路径白名单、请求改写、响应改写（同上）。
- 作为 MCP 工具暴露（没有参数 Schema，不实现 `ToolSource`）。
- WebSocket、gRPC。

## 2. 关键设计

### 2.1 没有按池配置，Key 的放置方式写在 Key 里

`protocol.Upstream` 只有 `BaseURL` 和 `Secret`，适配器拿不到任何池级配置（要加就得改冻结的 `internal/protocol`）。不同 REST API 放 Key 的位置又各不相同（Bearer、自定义头、查询参数、Basic）。

本卡的做法：**把放置方式编码在 Key 的明文里**，管理员添加 Key 时写成 `<方式>:<参数>`。Key 本来就加密存储、只在出站时解密，放置方式跟着 Key 走，不需要改契约。见 §3.3 与待定 1。

### 2.2 有副作用的方法不能重试

内核遇到 5xx、超时、网络错误会换 Key 重试（spec/07 §6）。对 `GET` 这是好事，但对 `POST` 可能造成重复下单、重复发短信。内核没有「按方法禁止重试」的开关，但有一条现成规则：**流式请求体（`BodyStream`）的请求只尝试一次**（CORE-07）。因此 `POST` / `PATCH` 路由声明为 `BodyStream`，自然只尝试一次；`GET` / `HEAD` / `PUT` / `DELETE` 是幂等的，可以重试。见待定 2。

### 2.3 403 不能当作 Key 失效

`protocol.ClassifyHTTP` 把 401 归为 `OutcomeAuthFailed`、403 归为 `OutcomePermissionDenied`，两者都会让调度器隔离这个 Key。对大模型 API 这是对的；但通用 REST API 常用 403 表示「这个资源你没权限」（例如访问别人的私有仓库），隔离 Key 会误伤所有客户端。本适配器在 `Do` 里把上游 403 改写为 `OutcomeClientError`，让内核把上游错误体原样转给客户端；401 保持不变（Key 确实失效）。见待定 3。

### 2.4 查询参数里的 Key 不能出现在日志里

Go 的 `*url.Error` 会把完整 URL 写进错误信息。Key 放在查询参数里时，网络错误的日志就会带出明文 Key（AGENTS.md §3 第 1 条）。适配器必须在把错误交给内核之前，把 `*url.Error.URL` 中的查询串替换掉。

## 3. 规格草案（阶段 A：改写 spec/08 §8.3 为正式规则，提交审批）

> 同时把 spec/08 §8 的标题「非大模型 API 接入范例（设计参考，尚未实现）」改为「非大模型 API 接入」：§8.1、§8.2 早已实现，标题过时。
> 规则编号前缀 `RS-`。

### 3.1 包与路由

| 编号 | 规则 |
|:--|:--|
| RS-01 | 包 `internal/adapter/rest`；`const Protocol protocol.Protocol = "rest"`；注册优先级与 `tavily` 相同，不是 Fallback；`Claims` 恒为 false |
| RS-02 | 路由（都是 `RouteCall`，无目录路由），路径一律为 `/rest/{service}/{path...}`：`GET` → Op `rest.get`，`BodyNone`（`http.ServeMux` 的 `GET` 模式同时匹配 `HEAD`，HEAD 请求也走这条路由，不单独声明）；`DELETE` → `rest.delete`，`BodyNone`；`PUT` → `rest.put`，`BodyBuffered`；`POST` → `rest.post`，`BodyStream`；`PATCH` → `rest.patch`，`BodyStream`。注册表要求 Op 在协议内唯一（REG-02），所以每个方法只有这一条路由；访问上游根路径用 `/rest/{service}/`（`{path...}` 可以为空）。*待定 2：POST / PATCH 是否只尝试一次* |
| RS-03 | `Envelope.Model` = `{service}`，即配置 `models[].name`；`X-UAIG-Model` 不适用（服务名已在路径里）。`{service}` 为空 → `CodeInvalidRequest` |
| RS-04 | `Params`：`method`（`r.Method` 原样，因此 HEAD 以 HEAD 转发）、`path`（`{path...}` 的**转义形式**，取自 `r.URL.EscapedPath()`，不含 `/rest/{service}` 前缀）、`query`（`r.URL.RawQuery` 原样） |
| RS-05 | 路径安全：`path` 按 `/` 拆段后，任一段在百分号解码后等于 `.` 或 `..`，或含 `\`、NUL → `CodeInvalidRequest`，消息 `path must not contain dot segments` |
| RS-06 | `Estimate = {requests: 1}`；无亲和线索 |

### 3.2 出站

| 编号 | 规则 |
|:--|:--|
| RS-10 | 上游 URL = `protocol.JoinURL(base_url, path)`，有 `query` 时追加 `?` + `query`。`Call.Model`（`upstream_model`）不参与 URL |
| RS-11 | 方法取 `Params["method"]`，只允许 `GET`、`HEAD`、`DELETE`、`PUT`、`POST`、`PATCH`，其他值 → `CodeInvalidRequest`；`PUT` 的请求体用 `Call.Body`，`POST` / `PATCH` 用 `Call.BodyStream`，其他方法无请求体 |
| RS-12 | 请求头白名单（HDR-02）：`Content-Type`、`Accept`、`Accept-Language`、`If-Match`、`If-None-Match`、`If-Modified-Since`、`If-Unmodified-Since`、`Range`。另外，客户端请求头 `X-UAIG-Forward-<Name>: <value>` 以 `<Name>: <value>` 转发，用于上游要求的自定义头（如 `X-Api-Version`）；`<Name>` 是下列之一时**丢弃**并记一次 Warn：`Authorization`、`Proxy-Authorization`、`Cookie`、`Host`、`Content-Length`、`Transfer-Encoding`、`Connection`、`Upgrade`、`TE`、`Trailer`、`Keep-Alive`、`X-Api-Key`、`X-Goog-Api-Key`、任何 `X-Forwarded-*` 与 `X-UAIG-*`。*待定 4：是否提供 `X-UAIG-Forward-*` 机制* |
| RS-13 | `User-Agent` 同 HDR-05 |
| RS-14 | 上游 Key 按 §3.3 放置；Key 放置在头里时，同名的客户端转发头被覆盖；放置在查询参数里时，客户端查询串中的同名参数被删除 |
| RS-15 | 2xx 响应一律 `ResponseRaw`，`Status` = 上游状态码；响应头白名单：`Content-Type`、`Content-Length`、`Content-Disposition`、`Content-Range`、`Accept-Ranges`、`Cache-Control`、`ETag`、`Last-Modified`、`Location`。上游是 `text/event-stream` 时同样按字节流转发，不解析 |
| RS-16 | 非 2xx：用 `protocol.Send` 的分类，但上游 **403** 改写为 `OutcomeClientError`（Status 保持 403），使内核原样转发错误体而不隔离 Key。*待定 3* 。错误体超过 8 KiB（`MaxErrorBodyBytes`）时被截断，这是已知限制，写进部署文档 |
| RS-17 | 网络错误：交给内核前，把 `*url.Error` 的 `URL` 字段替换为去掉查询串的 URL（无论 Key 放在哪里，一律去掉），避免日志泄露凭据或客户端参数 |
| RS-18 | `Units() = ["requests"]`；Meter 在 2xx 时报告 `{requests: 1}`（`UsageUpstream`），`Fault()` 恒为无 |

### 3.3 Key 的放置方式（`Secret` 的格式）

| 写法 | 上游请求 | 说明 |
|:--|:--|:--|
| `bearer:<token>` | `Authorization: Bearer <token>` | |
| `header:<Name>:<value>` | `<Name>: <value>` | `<Name>` 必须是合法的头名（RFC 9110 token），且不能是 RS-12 丢弃列表中除 `Authorization`、`X-Api-Key` 以外的名字 |
| `query:<name>:<value>` | 查询参数 `<name>=<value>`（URL 编码） | `<name>` 只允许 `[A-Za-z0-9_.-]` |
| `basic:<user>:<password>` | `Authorization: Basic base64(user:password)` | 密码中可以含 `:`（只按第一个 `:` 拆分 user） |
| 不以上述前缀开头 | 视为 `bearer:<整个明文>` | 兼容最常见的情况 |

| 编号 | 规则 |
|:--|:--|
| RS-20 | 格式不合法（如 `header:` 后缺少 `:`、头名非法）→ `Do` 返回 `*AttemptError{Outcome: OutcomeAuthFailed}`，错误信息只写「rest upstream key format is invalid」，**不含任何部分明文**。调度器因此隔离这个 Key，管理员会在 Key 列表里看到它 |
| RS-21 | 管理 API 添加 Key 时不校验格式（管理面不认识协议）；DEPLOYMENT.md 写明格式与示例 |

*待定 1：Key 放置方式的表达办法*，见 §3.4。

### 3.4 待定项汇总

| # | 问题 | 选项 | 建议 |
|:--|:--|:--|:--|
| 1 | Key 放在哪里由谁决定 | a. 编码在 Key 明文中（§3.3）；b. 注册 `rest_bearer`、`rest_header` 等多个协议名（`header` 方式仍无法指定头名）；c. 等按池配置的架构决策后再做 | **a**：不改契约，不同 Key 甚至可以用不同方式 |
| 2 | `POST` / `PATCH` 是否只尝试一次 | a. 是（`BodyStream`，CORE-07）；b. 否（`BodyBuffered`，失败可换 Key 重试，有重复副作用的风险） | **a**：通用转发不知道接口是否幂等，按最保守处理 |
| 3 | 上游 403 | a. 当作客户端错误原样转发，不隔离 Key；b. 与其他协议一致，隔离 Key | **a**：通用 API 的 403 多是资源级权限 |
| 4 | 自定义请求头 | a. 提供 `X-UAIG-Forward-<Name>`（RS-12）；b. 只转发白名单 | **a**：很多 API 要求版本头，没有它就只能写专用适配器 |

## 4. 实现要求（阶段 B：用户批准后）

### 4.1 文件布局

```
internal/adapter/rest/
  register.go      Protocol、Register
  ingress.go       Routes、Parse（RS-02 ~ RS-06）、WriteError、StreamError、WriteCatalog（404）
  egress.go        Prepare、Do（RS-10 ~ RS-18）、meter
  secret.go        Key 格式解析（§3.3、RS-20），返回「如何修改 *http.Request」的结构，不返回明文片段的错误
  *_test.go
  conformance_test.go   adaptertest.RunIngress / RunEgress
```

- **错误格式**（`WriteError`）：`{"error":{"code":"<apperr 码>","message":"<消息>"}}`，与管理面相同；`StreamError` 返回同样 JSON 的事件（不会被用到，因为响应不是 SSE，但接口要求实现）。
- `Prepare`：`Call = {Op, Model, Body: env.Body, BodyStream: env.BodyStream, Params: env.Params, Header: env.Header}`，不改写请求体。
- `Do` 使用 `protocol.Send`；拿到 `*AttemptError` 后按 RS-16 改写 403、按 RS-17 清理 URL，再返回。
- 2xx 时 `Response.Raw = resp.Body`，由内核负责读完与关闭。

### 4.2 装配与分层

- `cmd/gateway/adapters.go` 加一行 `rest.Register`。
- `internal/archtest/archtest_test.go` 的 `layers` 表登记 `"internal/adapter/rest": 2`（只加这一行，不改规则；本卡即为登记的批准）。

### 4.3 文档

- `spec/08_adapters.md` §8：标题改正，§8.3 改写为 §3 的正式规则。
- `docs/DEPLOYMENT.md` 新增一节「接入任意 REST API」：配置示例、Key 的四种写法、`X-UAIG-Forward-*` 用法、已知限制（只能按次计费；POST / PATCH 不重试；错误体超过 8 KiB 会截断；没有会话亲和）。
- `gateway.example.yaml` 加一个注释掉的 `rest` 池示例。

## 5. 涉及文件

- 阶段 A：`spec/08_adapters.md`、`spec/11_tasks.md`
- 阶段 B：
  - 新建：`internal/adapter/rest/`（上述文件）
  - 修改：`cmd/gateway/adapters.go`（一行）、`internal/archtest/archtest_test.go`（`layers` 表一行）、`docs/DEPLOYMENT.md`、`gateway.example.yaml`
  - 可选：`cmd/gateway/e2e_test.go` 之外新建 `cmd/gateway/e2e_rest_test.go`（不改现有用例）
- **不要改**：`internal/core`、`internal/protocol`、`internal/adapter/adaptertest`、其他适配器包

## 6. 测试要求

1. **一致性套件**：`adaptertest.RunIngress`、`adaptertest.RunEgress` 全部通过。
2. **路由**：5 条路由各自命中正确的 Op 与 `BodyMode`；HEAD 走 `rest.get` 且以 HEAD 转发、上游不返回响应体；`/rest/{service}/`（空子路径）转发到上游根路径；`Seal()` 通过（Op 唯一、无重复路由）；`Params` 中 `path` 保留百分号编码（如 `a%2Fb` 不被解码成两段）、`query` 原样。
3. **路径安全**（RS-05）：`..`、`.`、`%2e%2e`、`%2E.`、含 `\` 的段都返回 400；正常的 `v1/items/42` 通过。
4. **Key 格式**（§3.3）：五种写法各一个用例（检查上游实际收到的头或查询参数）；`basic` 密码含 `:`；`query` 方式覆盖客户端同名参数；`header` 方式覆盖同名转发头；格式非法时 `OutcomeAuthFailed`，且错误信息与日志中不含明文的任何片段。
5. **请求头**（RS-12）：白名单头被转发；客户端的 `Authorization`、`Cookie`、`X-Api-Key` 不被转发；`X-UAIG-Forward-X-Api-Version` 被转发为 `X-Api-Version`；`X-UAIG-Forward-Authorization`、`X-UAIG-Forward-Host`、`X-UAIG-Forward-X-Forwarded-For` 被丢弃。
6. **错误**：上游 404 / 400 / 422 原样转发状态码与错误体；403 原样转发且 Key **未被隔离**（端到端用例中检查 `GET /admin/api/keys` 的状态）；401 隔离 Key；429 / 5xx 对 `GET` 换 Key 重试，对 `POST` 只尝试一次（假上游计数）。
7. **凭据不泄露**（RS-17）：Key 放在查询参数里，让假上游在读到请求后直接断开连接，检查返回的错误与网关日志里都没有 Key 明文。
8. **响应**：二进制响应体逐字节一致；`Content-Disposition`、`ETag` 被转发，上游的 `Set-Cookie` 不被转发；上游 `text/event-stream` 按字节流转发。
9. **计费**：成功请求记 `requests: 1`；失败请求不计费（端到端用例中查 `usage_events`）。

## 7. 验收标准

- [ ] 阶段 A 的规格已获用户批准，4 个待定项都有答案。
- [ ] §6 的测试全部通过；`go run ./tools/check` 全绿。
- [ ] 没有修改 `internal/core`、`internal/protocol`、`internal/adapter/adaptertest`；`internal/archtest` 只在 `layers` 表加了一行；没有新增依赖。
- [ ] 日志、错误信息、响应中没有任何上游 Key 的明文或片段（AGENTS.md §3 第 1 条）。
- [ ] DEPLOYMENT.md 的新章节写明了 Key 格式与全部已知限制。
- [ ] 改完后在 Linux 上按 [T46](T46_race_verify_linux.md) 的流程复跑 `-race`（可以由审查方安排）。

## 8. 建议提交拆分

1. 阶段 A：`docs(spec): T51 通用 REST 转发适配器规格（待审批）`。**提交后停下等批准。**
2. `feat(rest): T51 通用 REST 转发适配器`：适配器包、一致性测试与单元测试。
3. `feat(gateway): T51 注册 rest 适配器并新增端到端用例`：`adapters.go`、`archtest` 一行、端到端用例。
4. `docs: T51 部署文档与配置示例`：DEPLOYMENT.md、`gateway.example.yaml`，`spec/11` 状态改为 `DONE`。
