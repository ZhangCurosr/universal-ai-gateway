---
name: add-api
description: 把一个外部 REST API 接入 universal-ai-gateway：查阅官方文档，生成池、服务与接口声明（models[].tools），校验并热重载，用 gateway keys import 导入密钥（密钥不经过对话），最后实际调用一次验证。当用户说「把 X API 接进网关」「给网关加一个服务」「让 AI 能调用 X」时使用。
---

# 接入一个 REST API

目标：用户说出一个 API（名字或文档链接），你把它接进网关，让它成为带钥匙池、计费和权限控制的服务，并且能被 AI 以 MCP 工具的方式调用。整个过程**不重启网关**，**密钥从不出现在对话里**。

规则依据：spec/08 §8.3（rest 适配器，含 §8.3.5 接口声明）、spec/03 CFG-17 / CFG-18（配置校验）与 §8（重载）、spec/14 CLI-09（密钥导入）。用户文档：docs/DEPLOYMENT.md §9、§5.1。

## 安全底线（任何一步都不能违反）

1. **不读取、不打印、不复述任何密钥。** 用户的账号文件里有密钥、邮箱、密码：不要用 Read、cat、head、grep 打开它的内容。只把路径交给 `gateway keys import`。需要知道文件格式时，请用户告诉你字段名，或者只读取字段名（例如 `python -c "import json;print(sorted(json.loads(open(p).readline())))"`，只输出键名，不输出值）。
2. **默认只开放只读接口。** 写操作（创建、修改、删除、发送、支付……）必须逐个列出并得到用户明确同意，才加进 `tools`。
3. **不确定是否有副作用的 POST，不要标 `idempotent: true`。** 只有查询、检索、打分这类确定没有副作用的 POST 才标。
4. **提醒用户：接口声明不是权限边界。** 被允许访问该服务的客户端，仍然可以直接请求 `/rest/{service}/` 下的任意路径，所以池里只能放最小权限的 Key（DEPLOYMENT.md §9 开头的安全警告）。
5. 管理员 Token 只通过环境变量 `UAIG_ADMIN_TOKEN` 使用，不写进文件，也不在输出中出现。

## 步骤

### 1. 确认前提

- 找到网关的配置文件（`gateway serve --config` 所用的那个，常见是 `gateway.yaml`），以及管理面地址（`UAIG_ADMIN_URL`，缺省 `http://127.0.0.1:8081`）。
- 确认 `UAIG_ADMIN_TOKEN` 已经在环境里（`test -n "$UAIG_ADMIN_TOKEN" && echo set`）；没有就请用户自己设置，不要让用户把 Token 贴到对话里。

### 2. 查文档

按顺序尝试：

1. OpenAPI / Swagger 描述：文档页上的链接，或常见路径 `/openapi.json`、`/swagger.json`、`/v3/api-docs`、`/docs/openapi.yaml`。
2. 官方 API 参考页面（HTML）。
3. 官方 SDK 源码中的端点定义。

需要弄清楚：

- **base URL**（含版本前缀，例如 `https://api.example.com/v2`）；
- **鉴权方式**，对应 Key 的写法（spec/08 §8.3.3）：`Authorization: Bearer` → `bearer:`；自定义头 → `header:<名字>:`；查询参数 → `query:<名字>:`；HTTP Basic → `basic:<用户>:<密码>`。签名类（AWS SigV4、HMAC）与需要自动刷新的 OAuth **不支持**，遇到就停下告诉用户；
- **计费**：缺省按次计费（`requests`），问用户每次调用的价格（不知道就用 `"0"`）。文档写明按页数、积分、字符数计费，且响应或请求里有对应字段时，用 `api.endpoints[].usage` 声明（第 4 步）；
- **错误码**：额度用完、Token 错误、任务不存在等是不是放在 HTTP 200 的响应体里（例如 `{"code": -60018}`）；额度什么时候重置；
- **异步任务**：有没有「提交 → 查询」两步、任务是否只能用提交它的 Key 查询；
- **候选接口**：方法、路径、参数、返回什么、有没有副作用。

### 3. 让用户挑接口

列一张表给用户：接口、用途一句话、只读还是写、你建议的 `idempotent`。默认只勾选只读接口。等用户确认后再继续。

### 4. 生成配置

在配置文件中追加一个池和一个服务（名称用小写和连字符，例如 `weather-pool` / `weather`）：

```yaml
pools:
  - name: weather-pool
    protocol: rest
    base_url: "https://api.weather.example/v2"

models:
  - name: weather
    pool: weather-pool
    price: { requests: "0.001" }
    reserve: { requests: 1 }
    tools:
      - name: weather_forecast            # ^[A-Za-z0-9_.-]{1,64}$，全网关唯一，建议「服务名_动作」
        description: "查询城市未来几天的天气预报，返回每天的最高温、最低温和降水概率。适合回答出行、穿衣类问题。"
        method: GET
        path: /forecast/{city}             # {x} 必须在 properties 与 required 中
        read_only: true
        input_schema:
          type: object
          properties:
            city: { type: string, description: "城市英文名，如 Paris" }
            days: { type: integer, minimum: 1, maximum: 7, description: "天数，缺省 3" }
          required: [city]
          additionalProperties: false
```

写 `tools` 时注意（CFG-17）：

- `description` 写给 AI 看：**什么时候用、返回什么**，一两句话；
- 参数映射是固定的：路径占位符取同名参数；`GET` / `DELETE` 的其余参数进查询串；`POST` / `PUT` / `PATCH` 的其余参数组成 JSON 请求体。上游要求的参数位置与此不同（例如参数必须放在请求头里）时，这个接口不适合声明，告诉用户；
- 每个参数写 `description`，能限制的都限制（`enum`、`minimum`、`required`、`additionalProperties: false`）；
- `idempotent: true` 只用于无副作用的 `POST` / `PATCH`；
- 二进制响应（图片、PDF）不支持作为工具结果。

文档里有业务错误码、按字段计费或异步任务时，在同一个服务上加 `api`（规则见 DEPLOYMENT.md §9.8，spec/08 §8.3.6）：

```yaml
    api:
      errors:
        - when: { json: code, in: [1001] }        # 值的类型与上游一致：数字写数字，字符串加引号
          outcome: rate_limited                   # 额度用完；Token 错误用 auth_failed；参数错误用 client_error
          cooldown_until: { time: "00:00", tz: "+08:00" }   # 额度每天几点重置；每月重置再加 day_of_month
      endpoints:
        - match: "POST /tasks"
          task_id: data.id                        # 提交接口：记下任务 ID
          usage: { pages: { json: data.pages, default: 1 } }
        - match: "GET /tasks/{id}"
          pinned_by: id                           # 查询接口：只用提交任务的 Key
          usage: {}                               # 查询不计费
```

- `usage` 里用到的单位必须写进 `price`；额度按天重置时，池上加 `cooldown: { max: 24h }`；
- 需要失败自动换 Key 或按请求体字段计费的 `POST` 接口，工具声明写 `idempotent: true`（走 `/rest-idem/`）；
- 不确定错误码含义时不要猜，宁可不写这条规则（未命中的 2xx 照常当作成功）。

文档要求「提交任务 → 轮询 → 下载结果」或「先申请上传地址再上传文件」时，用 `flow` 声明一个虚拟端点（DEPLOYMENT.md §9.8 后半部分，spec/08 §8.3.7），让 AI 一次调用拿到结果：`submit`（转发 JSON 请求体，走 `/rest-idem/`）或 `upload`（请求体是文件，走 `/rest/`），`poll` 写清状态字段与完成、失败的取值，`result` 写下载地址字段（结果是 zip 时加 `zip_entry`）。同步等待需要在池上加 `timeouts: { response_header_base: 10m }`。

接口的请求体是文件本身（上传类 flow，或直接 `POST` 文件的接口）时，给 AI 用的工具加 `file: { arg: file, name_query: name }`，并在配置中打开 `files.enabled`（DEPLOYMENT.md §9.9）：AI 先用 `gateway_upload_file`（本地桥加 `--upload-dir`）上传得到 `file_id`，再把它作为 `file` 参数传给工具。不要让 AI 把文件内容放进参数。

### 5. 校验并预览

```bash
gateway check-config --config gateway.yaml
gateway reload --dry-run
```

有错就按错误信息改配置，直到通过。dry-run 应该显示 `mode: hot`，`pools added` 与 `services added` 是你新加的名字。如果出现 `needs restart`，说明你改到了别的字段：停下检查。

### 6. 生效

```bash
gateway reload
```

### 7. 导入密钥（密钥不经过对话）

请用户提供账号文件的**路径**和格式（字段名、过滤条件），然后运行：

```bash
gateway keys import --pool weather-pool --file /path/to/accounts.jsonl \
  --field api_key --where status=ok --secret-prefix bearer: --name-prefix weather --dry-run
```

先 `--dry-run` 看计数，确认无误再去掉 `--dry-run` 执行。输出只有计数与行号，例如 `imported 12, duplicate 0, skipped 1 (filtered or invalid), failed 0, updated 0, expired 0`。每行一个密钥的文本文件用 `--format lines`。

账号文件里有过期时间字段时（只看字段名，不看值），加 `--expires-field <字段名>`，必要时加 `--tz +08:00`；过期账号会被跳过。可以用 `--label <来源>` 标记这一批 Key。账号续期后加 `--update` 重新运行。

用户只有一两把 Key、不想用文件时，请用户自己在管理页面（`http://127.0.0.1:8081/admin/ui/` → 上游 Key → 新建）粘贴，不要让用户把 Key 发给你。

### 8. 验证

需要一个允许访问该服务的客户端 Token（`UAIG_TOKEN`）。没有的话请用户在管理页面新建客户端（Token 只显示一次，由用户自己保存并设置到环境变量）。然后选一个只读接口实际调用一次：

```bash
gateway call weather_forecast --args '{"city":"Paris","days":1}'
```

成功即完成。失败时：`HTTP 401` → Key 写法或 Key 本身有误；`HTTP 403` → Key 权限不足（网关不会自动隔离，见 DEPLOYMENT.md §9.5）；`HTTP 404` → 路径或 base URL 有误；参数错误 → 按上游的错误消息修正 `input_schema` 或 `description`，重新 `gateway reload`。

### 9. 汇报

告诉用户：新增的服务名、开放的工具（只读 / 写）、导入的 Key 数量、验证结果，以及第 4 条安全底线的提醒。客户端要使用这个服务，需要把服务名加进它的 `allowed_models`（管理页面 → 客户端 → 编辑）。
