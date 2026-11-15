# T80 REST 池级密钥前缀（已批准，已完成）

审批：2026-11-15 用户确认「批准建议方案与文件范围，直接实施并重启验证」。

用户目标：修改网关代码，使已导入的 7 把 Deepgram 原始 Key 直接可用，部署后通过当前网关进行真实音频转录。
前置：T51 REST 适配器、T55 配置重载、T76 完整配置预检（均 DONE）。按 task-card 技能执行，一次只实现此卡；不提交或推送。

## 建议方案

```yaml
pools:
  - name: deepgram
    protocol: rest
    base_url: https://api.deepgram.com/v1
    secret_prefix: "header:Authorization:Token "
```

1. secret_prefix 是可选的池级字符串；没有配置时保持现有行为。只用于 rest 池，在装配/预检时验证，非 rest 池设置则报错。
2. 只给未使用 bearer:/header:/query:/basic: 显式格式的原始 Key 添加前缀。显式格式优先，避免已有 Key 重复包裹。非法格式及换行严格拒绝，不泄露值。
3. 支持已有格式所需的安全前缀：bearer:、header:<合法且允许的头名>:<固定前缀>、query:<合法参数名>:、basic:<用户名>:。校验放在 rest 适配器，未知前缀拒绝。配置前缀本身不得包含密钥。
4. 不按供应商域名猜测；在已知 Deepgram 池上配置一次。Speechmatics 等池维持默认 Bearer。
5. 利用已有 REST 按服务装配机制，将所属池的前缀交给适配器；内核、路由、调度器、protocol 冻结契约不变；不增加依赖。
6. 兼容新增字段：config.PoolConfig.SecretPrefix 与 rest.APIDef.SecretPrefix。已有导出函数/方法签名不变；本草案批准即授权上述两个导出结构体新增字段。
7. 覆盖原始 Key、显式格式优先、CRLF、危险头、未知前缀、非 REST 池拒绝、多池隔离、配置热重载与上传/转录完整路径，不修改已有测试期望。

## 文件范围

- internal/config/config.go、internal/config/config_test.go：配置字段与解析回归。
- internal/adapter/rest/api.go、secret.go、secret_test.go、egress.go、egress_test.go：前缀校验、鉴权与分支测试。
- cmd/gateway/rest_api.go、cmd/gateway/e2e_rest_api_test.go：池到 REST 服务的装配、完整转发及热重载测试（必要时在现有 e2e 测试文件内追加）。
- spec/03_config.md、spec/08_adapters.md、spec/11_tasks.md：新增规则、任务登记及状态；其余 spec 不改。
- docs/DEPLOYMENT.md、gateway.example.yaml、docs/handoff/T80_pool_secret_prefix.md：配置示例、正式任务卡与交付记录。
- .agents/skills/add-api/SKILL.md 及 .claude/skills/add-api/SKILL.md：引导使用池级前缀，仍可兼容单 Key 显式格式。

## 已知基线阻塞与处理顺序

实施前源码已有 T61 回归：新增数据库迁移 0005 后，backupSchemaVersion 仍为 4。该文件已有用户先前 T61 范围授权；仅 cmd/gateway/backup.go 的常量改为 5，保留版本守卫与全部测试，先修复并验证此基线问题，再实现 T80。该修复单独记录，不扩展备份行为。
新增 doctor 的既有测试在本机 8080 有运行服务时会误失败。完整检查期间正常停止当前网关，检查结束后恢复，不修改 doctor 测试与期望。

## 验收与部署

1. go run ./tools/check 全绿（含竞态、零 CGO、架构检查），保存最后 10 行。
2. 交叉构建 gateway.exe 与 WSL Linux 程序并做版本检查。
3. 备份当前配置及程序，通过新程序预检带 secret_prefix 的配置；优雅重启，失败则恢复旧程序与配置。
4. 使用相同的 8.49 秒本地生成测试语音，经当前网关验证 Deepgram 与 Speechmatics 均能返回转录文本，并核对任务使用同一 Key；不会读取/输出上游密钥。
5. 确认原有池、服务、客户端、Key、主密钥与用量历史保留；不更改 Key 的存储值。

## 公开辅助函数

适配器增加 `ValidateSecretPrefix` 供装配层对未被服务引用的池做预检，附直接单元测试。已有函数/方法签名、内核与 protocol 契约不变。

## 验收结果（2026-11-15）

- 满足：先将 T61 的 backupSchemaVersion 从 4 调整为 5，保留全部测试与 schema 守卫。停服后完整基线检查全绿，随后恢复旧实例。
- 满足：池字段、解析保留末尾空格、无前缀兼容、显式格式优先、非法前缀/控制字符/危险头、非 REST 与未引用池预检均有测试。
- 满足：适配器普通上传和 flow 的鉴权测试通过；完整网关测试验证同一上游地址的两个池隔离、原始 Key 原样导入、热重载后鉴权与二进制上传、拒绝非法重载及日志不出现测试 Key。
- 满足：go run ./tools/check 全绿，含 gofmt、vet、构建、零 CGO、技能同步与全部 race 测试。首次新回归用例误将注册表构造失败的响应预期为 400；根据既有重载契约改为 422，只修改本卡新增测试，原有测试期望不变。
- 满足：gateway.exe 与 Linux 产物均为 v0.0.0-8a3a377-t80-local，Windows 正式产物实际 version 检查通过。
- 满足：当前 WSL 网关已重启，新健康检查与 WebUI 返回 200，Deepgram 池仅新增 secret_prefix 配置。数据库升级到迁移 5；281 个上游 Key、1 个客户端及此前 34329 条用量历史保留，所有 Key 密文、包裹密钥、指纹、客户端 Token 哈希、管理员 Token 文件及主密钥保持不变。
- 满足：通过当前 127.0.0.1:8080 上传同一段本地生成的 8.49 秒、16 kHz、单声道 PCM WAV。Deepgram POST /rest/deepgram/listen 返回 200，耗时 5.477 秒；Speechmatics 提交返回 201，处理完成后取回文本 200，耗时 6.723 秒。两家文本与合成原文一致（单个测试样本，不代表整体识别率）。
- 满足：Speechmatics 提交、轮询和取回结果的审计记录均指向同一把 Key。两家均有 success 审计。
- 满足：未遗留本卡 TODO，未修改 internal/core、internal/protocol、archtest 与适配器套件，无新增依赖；用户于完成后要求自动提交并推送，现按该指令整理独立 T80 提交。

检查输出：/tmp/uai-deepgram-code-fix/final-check.log。
部署证据：/tmp/uai-deepgram-code-fix/deploy-result.json。
真实转录证据：/tmp/uai-deepgram-code-fix/audio-live/result.json（不含凭据）。

规格缺口：无。
限制：实测覆盖短英文 WAV 与单次转录；未验收长音频、实时 WebSocket 与全量 Key 的转录额度。测试消费少量真实上游额度；当前测试服务的网关价目仍为 0，不代表上游免费。未新增转录 MCP 工具，沿用已授权 REST 路由。
