# T49 管理 Web UI（M3）

- **类型**：规格 + 实现
- **前置**：无。只改管理面，不碰数据面和转译层，可以和 T47 / T48 并行。
- **需要用户审批**：**是**。spec/01 规定 M3 中除指标端点外的规格在 M2 完成后补充。本卡的阶段 A 就是补这部分规格，批准后才能写代码。
- **必读**：
  - [`spec/09_http_api.md`](../../spec/09_http_api.md) §2.2、§4（管理面 API 的端点、数据结构与行为）
  - [`spec/05_security_auth.md`](../../spec/05_security_auth.md) §4（ADM-01 ~ ADM-04）、§5（管理员引导）
  - [`spec/03_config.md`](../../spec/03_config.md) CFG-02（管理面只能监听回环地址）
  - [`spec/15_metrics.md`](../../spec/15_metrics.md)（`/admin/metrics` 不鉴权的理由）
  - [`internal/admin/`](../../internal/admin/) 全部文件，尤其是 `server.go` 的 `NewServer` 与 `wrap`
  - [`cmd/gateway/serve.go`](../../cmd/gateway/serve.go) 中管理面的装配
  - [`docs/DEPLOYMENT.md`](../DEPLOYMENT.md)
- **视觉参考**：[设计稿：网关管理控制台](https://claude.ai/artifact/53seA84EFwPgNjnKHn1GkY)（见 §1.1）

## 1. 背景与目标

现在管理网关只能用 `curl` 调用管理 API：创建客户端、发 Token、加上游 Key、看用量都要手写 JSON。本卡做一个**内嵌在网关二进制里**的管理页面，覆盖管理 API 已有的全部操作。

约束：

1. **不新增依赖，不引入前端构建链**。页面是手写的 HTML + CSS + 原生 JavaScript（ES 模块），用 `go:embed` 打进二进制。不用 npm、React、Vue、打包工具，也不从 CDN 加载任何东西。
2. **不改数据面，不改内核**。只改 `internal/admin` 和文档。
3. **页面能做的事 = 管理 API 能做的事**。需要新增 API 的地方都列在 §3.4 的待定项里，由用户决定。
4. **页面本身不是安全边界**。所有权限检查仍然在管理 API 里（ADM-01 ~ ADM-04）。页面只是 API 的客户端。

### 1.1 视觉参考

[设计稿](https://claude.ai/artifact/53seA84EFwPgNjnKHn1GkY)画了 6 个页面：登录、概览、客户端、新 Token 只显示一次、上游 Key、用量。实现时**布局、信息结构、文案和状态表达以设计稿为准**，以下几点除外：

| 设计稿里的做法 | 实现时 | 原因 |
|:--|:--|:--|
| 从 Google Fonts 加载 Inter、Noto Sans SC、JetBrains Mono | 用系统字体栈：`system-ui, -apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif`；等宽用 `ui-monospace, "SFMono-Regular", Consolas, monospace` | UI-03 的 CSP 禁止外部资源，约束 1 不许从外部加载任何东西 |
| 样式都写在元素的 `style` 属性里 | 全部放进 `style.css`，用类名 | UI-05 禁止内联样式 |
| 所有数据都是示例 | 全部来自管理 API | 约束 3 |
| 概览页的「计费队列」「审计丢弃」 | 只有待定 4 补了相应的只读 API 才显示，否则去掉 | 现有管理 API 不提供这两项；页面不能去抓 `/admin/metrics` 再自己解析（它是给 Prometheus 用的） |
| 只有中文 | 按待定 2 的决定 | |

设计稿里的颜色可以直接用：背景 `#F5F6F8`、卡片 `#FFFFFF`、边框 `#E5E7EB`、正文 `#111827`、次要文字 `#4B5563`、强调色 `#1D4ED8`。状态的配色是浅色底加深色字：可用为绿、冷却中为橙、已隔离为红、已停用为灰。这些组合都满足 4.5:1 的对比度。状态一律用「圆点 + 文字」表达，不能只靠颜色区分。

> 设计稿默认只有项目负责人能打开。开发者打不开时，向用户要共享权限，或者请用户导出截图放进交付分支的 `docs/handoff/assets/`。

## 2. 关键设计：用 Bearer Token，不用 Cookie

页面拿到管理员 Token 后，每次调用 API 都放在 `Authorization: Bearer` 头里，**不设置任何 Cookie**。

- 浏览器不会自动携带 Bearer 头，所以别的网站无法借用户的浏览器调用管理 API（CSRF 无从发生），不需要 CSRF Token。
- 管理面现有的鉴权逻辑完全不用改。
- Token 只保存在 `sessionStorage` 里：关掉标签页就没了，不写 `localStorage`，不写 Cookie。*（待定 1）*

静态资源（HTML / JS / CSS）本身**不需要鉴权**：里面没有任何数据或凭据，所有数据都要用 Token 调 API 才能拿到。

## 3. 规格草案（阶段 A：写进 spec 后提交审批）

> 建议在 `spec/09_http_api.md` 新增「§6 管理 Web UI（T49）」，并修改 spec/01 §2 的 M3 小节、`spec/11_tasks.md`（登记本卡）。标了「待定」的地方请用户回答。

### 3.1 路由与响应头

| 编号 | 规则 |
|:--|:--|
| UI-01 | 页面挂在管理面监听上：`GET /admin/ui/` 返回 `index.html`；`GET /admin/ui/<文件>` 返回内嵌的静态文件；`GET /admin/ui`（没有结尾斜杠）重定向到 `/admin/ui/`；内嵌文件里没有的路径返回 404；非 GET / HEAD 返回 405 |
| UI-02 | 静态资源不经过 ADM-01 鉴权，也不计入 ADM-04 的失败次数（在 `wrap` 中和 `/admin/metrics` 一样提前放行） |
| UI-03 | 每个响应都带：`Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、`X-Frame-Options: DENY`。`index.html` 加 `Cache-Control: no-store`，其他文件 `Cache-Control: no-cache` |
| UI-04 | `Content-Type` 按扩展名显式设置（`.html` / `.js` / `.css` / `.svg`），不依赖内容嗅探 |
| UI-05 | 页面里**不允许**内联脚本、内联样式、`eval`、`new Function`（UI-03 的 CSP 会拦下它们，开发时就能发现） |
| UI-06 | 内嵌资源总大小不超过 300 KiB（测试检查），防止误把大文件打进二进制 |

### 3.2 页面功能

| 页面 | 内容 | 调用的 API |
|:--|:--|:--|
| 登录 | 输入管理员 Token，调 `/admin/api/health` 验证；401 / 403 / 429 分别给出明确提示 | `GET /admin/api/health` |
| 概览 | 版本、运行时长；各池 Key 的数量与状态分布（ready / quarantined / disabled / 冷却中 / 熔断打开）；最近 24 小时的请求数与花费合计 | `health`、`GET keys`、`GET usage` |
| 客户端 | 列表（名称、角色、额度、已花费、预留、RPM、允许的模型、是否禁用、Token 显示值）；新建；编辑；禁用 / 启用；轮换 Token | `GET/POST/PATCH clients`、`rotate-token` |
| 上游 Key | 列表（池、名称、`secret_hint`、状态与原因、权重、并发上限、在途数、冷却秒数、熔断状态）；新建；编辑名称、权重、并发上限；设为 ready / disabled | `GET/POST/PATCH keys` |
| 用量 | 按客户端与时间范围查询；表格展示每个（客户端，模型）的请求数、各单位用量、花费；页尾合计 | `GET usage` |

| 编号 | 规则 |
|:--|:--|
| UI-10 | **新 Token 只显示一次**：创建客户端、轮换 Token 后，用一个对话框显示 `token`，提供复制按钮并提示「关闭后无法再次查看」。Token 不写进 `sessionStorage`、URL、控制台日志 |
| UI-11 | 上游 Key 的 `secret` 只在新建表单里出现，用 `type="password"` 输入框；提交后立即清空，不回显 |
| UI-12 | 所有来自 API 的字符串（客户端名称、Key 名称、状态原因、模型名……）一律用 `textContent` 或 `setAttribute` 写入页面，**禁止**用 `innerHTML` 拼接数据（防 XSS） |
| UI-13 | 金额：API 用 nano-USD 整数。显示时换算为美元，保留 6 位小数；输入时用户填美元小数，用**字符串运算**转成 nano-USD 整数，不经过浮点数。超过 `Number.MAX_SAFE_INTEGER`（约 900 万美元）的值在页面上标注「精度可能丢失」 |
| UI-14 | 时间：API 是 UTC 的 RFC 3339 字符串，页面按浏览器本地时区显示，鼠标悬停显示原始 UTC 值；用量查询的时间输入转成 UTC 再发出 |
| UI-15 | 危险操作要二次确认：禁用客户端、轮换 Token、把 Key 设为 disabled。ADM-03（最后一个管理员）返回的 409 原样展示 API 的错误消息 |
| UI-16 | API 返回 401 时清除 `sessionStorage` 中的 Token 并回到登录页；其他错误在页面上显示 `error.code` 与 `error.message`，以及响应头 `X-Request-Id`（方便对照服务端日志） |
| UI-17 | 上游 Key 页面每 10 秒自动刷新一次运行时状态，页面不可见（`document.hidden`）时暂停 |
| UI-18 | 界面语言：*待定 2* |

### 3.3 安全补充

| 编号 | 规则 |
|:--|:--|
| UI-20 | 管理面仍然只监听回环地址（CFG-02 不变）。远程使用的方式写进部署文档：SSH 端口转发（`ssh -L 8081:127.0.0.1:8081 host`），**不要**把管理面通过反向代理暴露到公网 |
| UI-21 | *待定 3：DNS 重绑定防护*。恶意网页可以把自己的域名解析到 `127.0.0.1`，从而在浏览器里访问本机的管理面。管理 API 需要 Bearer Token，攻击者拿不到，所以不受影响；但 `/admin/metrics` 不鉴权，指标数据可能被读走。建议：管理面对**所有**请求检查 `Host` 头，主机名只允许 `127.0.0.1`、`[::1]`、`localhost`，以及配置项 `server.admin_hosts` 中列出的值，其他一律 421。这会多一个配置项（spec/03） |

### 3.4 待定项汇总

1. **Token 存放位置**：a. `sessionStorage`（建议，关掉标签页即失效）；b. 只放在内存里（刷新页面就要重新输入）；c. `localStorage`（不建议，长期留在磁盘上）。
2. **界面语言**：a. 只做中文；b. 中英双语，按浏览器语言切换（文案集中在一个 `i18n.js` 里）。建议 b，工作量多半天。
3. **DNS 重绑定防护**（UI-21）：建议做。
4. **要不要补管理 API**？页面要做好下面两件事，需要新增只读端点：
   - `GET /admin/api/config`：返回配置中的池（名称、协议）与模型（名称、所在池），**不含** `base_url` 以外的任何敏感字段。新建 Key 时的「池」下拉框、编辑客户端时的「允许的模型」多选框都需要它。没有这个端点，就只能让用户手填，填错了由 API 返回 400。建议补。
   - `GET /admin/api/health` 增加两个字段：`ledger_queue: {length, capacity}`、`audit_dropped`，数据源与 spec/15 的对应仪表相同。只**增加**字段，不改已有字段。设计稿概览页的「计费队列」和审计丢弃提示需要它。建议补。
   - `GET /admin/api/audit?...`：查询审计日志。建议**本卡不做**，另开卡。

## 4. 实现要求（阶段 B：用户批准后）

### 4.1 文件布局

```
internal/admin/ui/            静态资源（go:embed 的根目录）
  index.html
  app.js                      入口：路由（location.hash）、登录状态
  api.js                      fetch 封装：加 Authorization 头、超时（AbortController，15 秒）、错误归一化
  pages/overview.js
  pages/clients.js
  pages/keys.js
  pages/usage.js
  money.js                    nano-USD 与美元字符串互转（UI-13）
  i18n.js                     （待定 2 选 b 时）
  style.css
internal/admin/ui.go          //go:embed ui；路由与响应头（UI-01 ~ UI-06）
internal/admin/ui_test.go
```

- 页面内路由用 `location.hash`（`#/clients`），服务端不需要 SPA 回退路由。
- `fetch` 一律带超时（AbortController），不留没有超时的请求。
- 不引入任何第三方 JS 或 CSS，也不从 CDN 加载字体。
- 表格数据量：客户端和 Key 都是一次性全部返回（API 没有分页），页面直接渲染。超过 500 行时只渲染前 500 行，并提示「请用筛选框缩小范围」。

### 4.2 `internal/admin` 的改动

- `NewServer` 中挂载 `/admin/ui` 与 `/admin/ui/`；`wrap` 中对这两个前缀和 `/admin/metrics` 一样提前放行（UI-02）。
- 待定 3 选「做」时：`wrap` 的最前面加 `Host` 检查；配置项 `server.admin_hosts` 加进 `internal/config`（spec/03 同步）。
- 待定 4 选「补」时：新增 `GET /admin/api/config`，走正常的管理员鉴权，数据结构写进 `types.go`；`health` 的新字段通过 `admin.Deps` 注入取数函数（与 T45 的 `metrics.Gauges` 同样的做法），由 `cmd/gateway/serve.go` 装配。`health` 已有的测试断言不改，只新增用例。
- 管理面的 `/` 兜底路由不变，`GET /` 仍然返回 JSON 404。启动日志加一行：`admin ui: http://<admin_listen>/admin/ui/`。

### 4.3 文档

`docs/DEPLOYMENT.md` 新增「管理页面」一节：怎么打开；首次登录用引导管理员的 Token（spec/05 §5）；SSH 端口转发；为什么不要暴露到公网；忘记管理员 Token 怎么办（沿用现有的恢复流程）。

## 5. 涉及文件

- spec：`spec/09_http_api.md`（新增 §6）、`spec/01_scope_and_architecture.md`（M3 小节）、`spec/11_tasks.md`（登记本卡与状态）；待定 3 选「做」时还有 `spec/03_config.md`
- 新增：`internal/admin/ui/**`、`internal/admin/ui.go`、`internal/admin/ui_test.go`
- 修改：`internal/admin/server.go`、`internal/admin/types.go`（待定 4）、`cmd/gateway/serve.go`（待定 4 的取数函数装配）、`internal/admin/server_test.go`（只新增用例）、`docs/DEPLOYMENT.md`；待定 3 选「做」时还有 `internal/config/*.go`、`gateway.example.yaml`

## 6. 测试要求

**Go 测试**（`go run ./tools/check` 覆盖）：

1. UI-01：`/admin/ui` 重定向；`/admin/ui/` 返回 `index.html`；每个内嵌文件都能取到且 `Content-Type` 正确；不存在的文件 404；`POST` 405；`/admin/ui/../api/clients` 这类路径不能绕到别的处理器，也不能读到内嵌目录以外的文件。
2. UI-02：不带 Token 能取到静态资源；连续请求静态资源不会触发 ADM-04 锁定；不带 Token 调 `/admin/api/*` 仍然 401。
3. UI-03：每个静态响应都带全部安全头，CSP 字符串与规格逐字一致。
4. UI-05、UI-06：扫描内嵌的 `.html` 文件，断言没有内联 `<script>` 内容、没有 `style=` 属性、没有 `on*=` 事件属性；扫描 `.js` 文件，断言没有 `innerHTML`、`eval(`、`new Function`、`localStorage`；内嵌资源总大小 ≤ 300 KiB。
5. 待定 3、4 的新逻辑各有测试：Host 检查的放行与 421；`/admin/api/config` 的鉴权，以及返回内容里没有密钥和 Key 信息。

**浏览器验证**（写进交付说明，附截图）：

6. 用真实浏览器打开页面，走一遍：登录 → 新建客户端（复制 Token）→ 用这个 Token 调一次数据面 → 用量页能看到这次请求 → 新建上游 Key → 改权重 → 设为 disabled → 轮换 Token → 旧 Token 失效。
7. 浏览器控制台没有 CSP 报错，没有 JS 报错。
8. 把客户端名称设成 `<img src=x onerror=alert(1)>`，页面原样显示文本，不执行脚本。
9. 金额输入 `0.000001` 美元，提交后 API 收到 `1000` nano-USD；输入 `12.3456789` 时提示超过 6 位小数。
10. 窄屏（宽 375px）下表格能横向滚动，没有内容被截断到看不见。

## 7. 验收标准

- [ ] 阶段 A 的规格已获用户批准，4 个待定项都有答案。
- [ ] §6 的测试全部通过；`go run ./tools/check` 全绿。
- [ ] 没有新增 `go.mod` 依赖；没有 npm / 构建步骤；没有从外部网络加载任何资源。
- [ ] 没有修改 `internal/core`、`internal/protocol`、数据面代码；没有改变现有管理 API 的行为（现有测试全部通过，期望值没有修改）。
- [ ] 浏览器验证的截图和步骤已附在交付说明里，截图中**没有**完整的 Token 或上游 Key。
- [ ] 部署文档的「管理页面」一节写完。

## 8. 建议提交拆分

1. 阶段 A：`docs(spec): T49 新增 spec/09 §6 管理 Web UI（待审批）`
2. `feat(admin): T49 内嵌静态资源与安全响应头`
3. `feat(admin): T49 管理页面：登录、概览、客户端、上游 Key、用量`
4. （按待定项）`feat(admin): T49 Host 检查` / `feat(admin): T49 只读配置端点`
5. `docs(deploy): T49 管理页面使用说明`
