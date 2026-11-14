# T73 `gateway connect`：一条命令把网关接入各家 AI CLI（已批准，已完成）

- **类型**：规格 + 实现。2026-11-14 用户批准全部建议（「全部按建议」）。
- **前置**：T69（`DONE`）。
- **约束**：不改 `internal/core` 与 `internal/protocol` 的导出 API；不改分层规则；**不加依赖**（没有 TOML 库：TOML 用带标记的文本块写入）；不用 CGO。

## 1. 要解决的问题

用户在别的项目里会用 Claude Code、Codex、OpenCode、Pi 等不同的 AI CLI。每个 CLI 接网关都要手工配三样东西，而且格式各不相同：

| CLI | 模型后端 | MCP | 技能目录 |
|:--|:--|:--|:--|
| Claude Code | 环境变量 `ANTHROPIC_BASE_URL`、`ANTHROPIC_AUTH_TOKEN` | `.mcp.json`（支持 `${VAR}`） | `.claude/skills`、`~/.claude/skills` |
| Codex | `config.toml` 的 `[model_providers.*]`（`env_key`、`wire_api = "responses"`） | `config.toml` 的 `[mcp_servers.*]`（`bearer_token_env_var`） | `.agents/skills`、`~/.agents/skills` |
| OpenCode | `opencode.json` 的 provider（`{env:VAR}`） | `opencode.json` 的 `mcp`（`{env:VAR}`） | `.agents/skills`、`.claude/skills`、`.opencode/skills` |
| Pi | `~/.pi/agent/models.json`（`apiKey: "$VAR"`） | 不内置（用命令行 `gateway call`） | `.agents/skills`、`~/.agents/skills` |

本卡提供 `gateway connect <cli>`：输出（或写入）该 CLI 的全部配置，并安装 `use-gateway` 技能。**Token 永远不写进任何文件**，配置里只引用环境变量 `UAIG_TOKEN`。

## 2. 命令

```
gateway connect <claude|codex|opencode|pi> [--scope project|user] [--write] [--model <服务名>]
gateway connect list                      # 列出支持的 CLI 与各自会写的文件
```

- 读取 `UAIG_URL`（缺省 `http://127.0.0.1:8080`）。`UAIG_TOKEN` 只在运行时用来查 `/v1/models` 填模型列表；没有设置时用占位符并提示。
- 不加 `--write`：只把要写的内容按文件打印出来（可以复制），什么都不改。
- `--scope project`（缺省）写当前目录的项目配置；`--scope user` 写用户目录（Pi 只有用户级 `models.json`）。
- `--model`：缺省模型（Codex 的 `model`、OpenCode 的 `model`）；不给时取 `/v1/models` 的第一个大模型服务，查不到就留占位符。

## 3. 规则

| 编号 | 规则 |
|:--|:--|
| CN-01 | 输出与写入的内容中只出现 `UAIG_TOKEN` 这个变量名，**从不出现 Token 的值**；命令自身的输出、错误信息也不出现（沿用 CLI 的 `redact`）。最后一行提示如何长期设置环境变量（Windows：`setx UAIG_TOKEN …`，其他：写进 shell 配置），由用户自己执行 |
| CN-02 | **claude**：模型后端用环境变量（Claude Code 的 `settings.json` 里的 `env` 会以明文保存 Token，所以不写），打印 `ANTHROPIC_BASE_URL=<URL>`、`ANTHROPIC_AUTH_TOKEN=$UAIG_TOKEN` 的设置方法；MCP 写 `.mcp.json`（project）或打印 `claude mcp add --scope user …`（user），条目 `uaig`：`{"type": "http", "url": "<URL>/mcp", "headers": {"Authorization": "Bearer ${UAIG_TOKEN}"}}`；技能装到 `.claude/skills/use-gateway/` 或 `~/.claude/skills/use-gateway/` |
| CN-03 | **codex**：写 `.codex/config.toml`（project）或 `~/.codex/config.toml`（user），内容放在标记块 `# >>> uaig (gateway connect) >>>` … `# <<< uaig <<<` 之间：`[model_providers.uaig]`（`name`、`base_url = "<URL>/v1"`、`env_key = "UAIG_TOKEN"`、`wire_api = "responses"`）与 `[mcp_servers.uaig]`（`url = "<URL>/mcp"`、`bearer_token_env_var = "UAIG_TOKEN"`）。`model_provider = "uaig"` 与 `model` 是顶层键，写在标记块里会被前面的表吞掉，所以**只打印、不写入**，提示用户放到文件开头或用 `codex -c model_provider=uaig`。技能装到 `.agents/skills/` 或 `~/.agents/skills/` |
| CN-04 | **opencode**：`opencode.json`（project）或 `~/.config/opencode/opencode.json`（user）。provider `uaig`：OpenAI 兼容，`baseURL = "<URL>/v1"`，`apiKey = "{env:UAIG_TOKEN}"`，`models` 为 `/v1/models` 的服务名；MCP `uaig`：remote，`url = "<URL>/mcp"`，`headers.Authorization = "Bearer {env:UAIG_TOKEN}"`，`oauth: false`。OpenCode V1 与 V2 的配置结构不同（V2 用 `providers` / `settings.baseURL`），`--opencode-schema v1|v2`（缺省按 `opencode --version` 判断，判断不了用 v2）。技能装到 `.agents/skills/` 或 `~/.agents/skills/` |
| CN-05 | **pi**：`~/.pi/agent/models.json` 的 `providers.uaig`：`baseUrl = "<URL>/v1"`、`api = "openai-completions"`（`--pi-api anthropic-messages` 时 `baseUrl = "<URL>"`）、`apiKey = "$UAIG_TOKEN"`、`models` 为服务名列表。Pi 不内置 MCP：不写 MCP，提示 `gateway tools` / `gateway call`（技能里已有用法）。技能装到 `~/.agents/skills/`（user）或 `.agents/skills/`（project） |
| CN-06 | 写入规则（`--write`）：文件不存在 → 新建（目录一并创建）。**JSON 文件已存在**：能按标准 JSON 解析时只增改 `uaig` 这一个条目，其他内容保持原样（按原文件的键顺序与 2 空格缩进重新输出）；含注释（JSONC）或解析失败时**不写**，打印片段让用户手工合并。**TOML**：替换已有的标记块，没有就追加到文件末尾。写之前把原文件备份为 `<文件>.bak`；同一内容再写一次不产生变化（幂等） |
| CN-07 | 技能：`use-gateway` 打包进 `gateway` 程序（`embed`），安装时整目录覆盖 `use-gateway/`（只动这个目录）；技能文件里记录安装它的 `gateway` 版本。`--write` 之外不安装 |
| CN-08 | 结束时打印验证命令：`curl -s $UAIG_URL/v1/models -H "Authorization: Bearer $UAIG_TOKEN"`，以及该 CLI 里的一句自检（如在 Codex 中选择 `uaig` 提供方后问一句 "ping"） |
| CN-09 | 用户级配置目录按各 CLI 自己的环境变量查找（与 CLI 的行为一致）：Codex `CODEX_HOME`（缺省 `~/.codex`）、OpenCode `XDG_CONFIG_HOME`（缺省 `~/.config`）、Pi `PI_CODING_AGENT_DIR`（缺省 `~/.pi/agent`，`models.json` 总是写在这里）、Claude Code `CLAUDE_CONFIG_DIR`（缺省 `~/.claude`，用于技能目录）。实测中发现：设置了 `PI_CODING_AGENT_DIR` 时 Pi 不读 `~/.pi/agent/models.json` |

## 4. 改动文件

- `cmd/gateway/connect.go`（新增）：子命令、四个 CLI 的生成器、JSON 合并、TOML 标记块；`connect_test.go`。
- `cmd/gateway/skills/use-gateway/SKILL.md`：从 `docs/skills/use-gateway/` 移来（`embed` 只能读包目录内的文件）；`docs/AI_QUICKSTART.md`、`docs/DEPLOYMENT.md` 的链接改为「运行 `gateway connect <cli> --write` 安装」。
- `cmd/gateway/main.go`（用法）、`client_cmds.go`（共用 `UAIG_URL`）。
- 规格：spec/14 新增 §7「接入各家 CLI」（CN-01 ~ CN-08）、spec/11 新增 T73；`docs/AI_QUICKSTART.md` 新增一节「把网关配成各 CLI 的模型后端」（就是本命令的输出样例）。

## 5. 测试

- 每个 CLI × 两种范围的生成结果（黄金文件比较）；输出中不出现 Token 值（测试里设置一个已知 Token 并搜索它）。
- JSON 合并：新建、已有其他条目（保留且顺序不变）、已有旧的 `uaig` 条目（替换）、JSONC（拒绝写入）、无法解析（拒绝写入）、幂等。
- TOML：追加、替换已有标记块、标记块之外的内容原样保留、幂等。
- 技能安装：目录覆盖、只动 `use-gateway/`。
- `/v1/models` 查不到（无 Token、网关不可达）时用占位符并提示，退出码 0。

## 6. 实测（验收）

本机网关 + 真实上游，各 CLI 在临时目录中：

1. `gateway connect codex --write`：Codex 选择 `uaig` 提供方，`codex exec "ping"` 有回复（走 `/v1/responses`），能看到 `uaig` 的 MCP 工具。
2. `gateway connect opencode --write`：`opencode run "ping"` 用 `uaig/<服务名>` 有回复；MCP 工具可见。
3. `gateway connect pi --write`：Pi 中 `/model` 能选到 `uaig` 的模型并回复；技能列表里有 `use-gateway`。
4. `gateway connect claude --write`：Claude Code 中 MCP 工具可见，技能可用。
5. 所有写入的文件中没有 Token 值；`.bak` 是修改前的内容。

（本机没有安装的 CLI 跳过，交付说明里写明。）

### 6.1 实测结果（2026-11-14，通过）

本机已安装的四个 CLI：Claude Code 2.1.316、Codex 0.166.0、OpenCode 1.19.5、Pi 1.5.2。本机没有大模型的真实 Key，所以用一个本地假上游（OpenAI Chat 与 Anthropic Messages 两种协议，总是回答 `pong`）验证「CLI → 网关 → 上游」整条链路。每个 CLI 都在临时用户目录中运行，没有改动用户真实的配置。

- [x] **Codex**：`connect codex --scope user --write` 写出 `config.toml` 标记块；`codex exec -c model_provider=uaig -m fake-llm` 经 `/v1/responses`（转译到 OpenAI Chat 上游）回答 `pong`；`codex mcp list` 显示 `uaig`（Bearer Token 取自 `UAIG_TOKEN`）。
- [x] **OpenCode**：1.19.5 按版本选择 V1 结构；`opencode run -m uaig/fake-llm` 回答 `pong`；`opencode mcp list` 显示 `uaig` 已连接。
- [x] **Pi**：`pi --provider uaig --model fake-llm -p` 回答 `pong`。第一次失败（`Unknown provider "uaig"`）：本机设置了 `PI_CODING_AGENT_DIR`，Pi 不读 `~/.pi/agent/models.json`。改为按各 CLI 的配置目录环境变量写入（CN-09）后通过。
- [x] **Claude Code**：`.mcp.json` 写入，`claude mcp list` 显示 `uaig`（等待用户在 Claude Code 中批准，属正常）；`ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` 指向网关后，`claude -p` 对 Anthropic 协议的服务回答 `pong`。
- [x] 所有写入的文件与命令输出中都没有 Token 的值；技能 `use-gateway` 装到各 CLI 的技能目录。
- [x] T74：同一网关上 `gateway admin call list_keys` 与 `gateway reload` 正常。

实测中发现的**不属于本卡**的问题（另开卡处理）：

1. Claude Code 2.1.316 会在 `messages` 数组里发送 `role: "system"` 的消息；转译到其他协议（如 OpenAI Chat 上游）时网关返回 400 `messages[1].role must be user or assistant`。对 Anthropic 协议的服务直接透传，不受影响。
2. Codex 经 `/v1/responses` 转译时打印 `OutputTextDelta without active item`（回答正常）：转译生成的 Responses 事件在文本增量之前缺少 `response.output_item.added` / `content_part.added`。

## 7. 决策（已批准）

| # | 问题 | 选项 | 建议 |
|:-:|:--|:--|:--|
| 1 | Token 放在哪里 | A. 只引用环境变量 `UAIG_TOKEN`，由用户用 `setx` 等自己设置；B. 写进各 CLI 的配置文件 | **A**：配置文件常被提交或同步，明文 Token 容易泄露；四个 CLI 都支持引用环境变量 |
| 2 | 已有的 JSON 配置文件怎么改 | A. 能解析就只改 `uaig` 条目，JSONC 或解析失败就只打印；B. 一律只打印，不改已有文件 | **A**：大多数情况能一步到位；带注释的文件不碰，避免丢注释 |
| 3 | TOML 怎么写（没有 TOML 库，也不能加依赖） | A. 带标记的文本块，顶层键只打印；B. 自己写一个 TOML 解析器 | **A**：块只包含两个表，替换安全；顶层键位置敏感，交给用户 |
| 4 | `use-gateway` 技能的来源 | A. 打包进 `gateway` 程序，`connect --write` 时安装，版本与网关一致；B. 仍放在 docs，手工复制 | **A**：网关更新后再运行一次就更新技能，不会和网关版本不一致 |
| 5 | Pi 用哪种协议 | A. 缺省 `openai-completions`，可选 `anthropic-messages`；B. 只支持一种 | **A**：两种网关都支持；缺省用最通用的那种 |
| 6 | 是否顺带写管理面 MCP | A. 不在本卡（T74 提供管理面命令行，配置另加 `--admin`）；B. 一起写 | **A**：管理员 Token 权限大，只应在维护网关的那台机器上配置，不应随每个项目一起写 |
