# T76 `gateway check-config` 做完整的启动前检查（已批准，已完成）

- **类型**：规格 + 实现（小卡，修复）。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：T72（`DONE`，已有「解析 + 构造一代」的检查）。
- **约束**：不改 `internal/core`、`internal/protocol` 的导出 API；不加依赖。`check-config` 不打开数据库、不监听端口、不连接上游、不写任何文件。

## 1. 问题

`gateway check-config` 现在只做 `config.Load`（YAML 解析 + CFG 规则）。有几类错误要到 `gateway serve` 构造注册表与内核时才报出来，实测中反复踩到：

| 例子 | 现在 | 应该 |
|:--|:--|:--|
| `openai_chat` 服务缺少 `cache_read_tokens` 的价格 | `config ok`，启动时 `core.New` 失败（CORE-05） | `check-config` 就报错并指明服务与单位 |
| `mineru` 服务缺少 `requests` 的价格 | 同上 | 同上 |
| rest 服务的 `api.endpoints[].usage` 用了某个单位但 `price` 里没有 | 同上 | 同上 |
| 接口声明用在不支持的协议上（CFG-18） | 启动时注册失败 | `check-config` 报错 |
| `alerts.webhook.url_file` 读不到或不是 http(s) 地址（CFG-22） | 启动时失败 | `check-config` 报错 |

## 2. 规则

| 编号 | 规则 |
|:--|:--|
| CK-01 | `check-config` 依次：① `config.Load`；② 构造一代（注册表 → 价目单位与接口声明 → 内核），与 `serve` / 重载 / T72 的「检查」使用同一函数；③ 告警推送地址文件检查（CFG-22）。任何一步失败：stderr 输出错误（与 `serve` 启动失败时的消息相同），以 1 退出；全部通过输出 `config ok`，以 0 退出 |
| CK-02 | 构造检查用一套只在内存中的依赖（空的账本、调度器、限流器），不打开 `data_dir` 中的数据库、不读主密钥、不监听端口、不访问上游。不需要 `data_dir` 已经存在 |
| CK-03 | 错误消息指明位置，例如 `model "deepseek-chat": price for unit "cache_read_tokens" is missing (pool "deepseek" uses protocol openai_chat)`；若现有的 CORE-05 消息不含池与协议，补上 |
| CK-04 | `--json`：输出 `{"ok": bool, "error": "..."}`，供脚本与 AI 使用 |

## 3. 改动文件

- `cmd/gateway/main.go`（`check-config`）、新增 `cmd/gateway/check_config.go`（内存依赖的构造检查，T72 的 `configEntryEditor.apply` 改为复用同一函数）及测试。
- 必要时 `internal/core` 的 CORE-05 错误消息补充池与协议（只改消息文本，不改导出 API）。
- 规格：spec/03 §1 `check-config` 的说明与 CK-01 ~ CK-04；spec/11 T76。
- 技能：`live-test` 去掉「`check-config` 查不出来」的那几条坑，改为「先跑 `check-config`」。

## 4. 测试

- 上表每一行一个用例：`check-config` 以 1 退出并给出位置；修正后 `config ok`。
- `data_dir` 不存在时也能检查；检查后不产生任何文件。
- `--json` 两种结果。

### 4.1 结果（2026-11-14）

- [x] 四类原来要到启动时才报的错误（缺 `cache_read_tokens`、缺 `requests`、接口声明用在不支持的协议上、告警地址文件不存在）都由 `check-config` 以 1 退出并指明位置；修正后 `config ok`；`data_dir` 与 `secrets` 不会被创建；`--json` 两种结果。
- 现有 CORE-05 的消息已经包含服务名、协议与单位，未修改 `internal/core`。
- T72 的「检查」与 `check-config` 共用 `validateGeneration`（同时补上了告警地址文件的检查）。

## 5. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | 检查到什么程度 | A. 构造一代 + 告警地址文件（不碰数据库、端口、上游）；B. 再加上探测端口能否监听、数据目录能否写 | **A**：B 的结果依赖运行时环境（端口可能被正在运行的网关占用），容易误报；A 覆盖了所有「配置本身写错」的情况 |
| 2 | 是否新增 `--json` | A. 新增；B. 不加 | **A**：AI 和脚本不用解析自然语言 |
