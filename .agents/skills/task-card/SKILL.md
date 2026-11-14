---
name: task-card
description: 在 universal-ai-gateway 仓库中按任务卡开发：起草规格（docs/handoff/Txx_*.md）→ 用户批准 → 实现 → 完整检查 → 实测 → 交付说明 → 用户要求时提交。用户说「起草 Txx」「做 Txx」「按建议实现」「提交」「拆成两个提交」，或要在本仓库新增 / 修改功能时使用。
---

# 任务卡开发流程（universal-ai-gateway）

`AGENTS.md` 是硬规则，优先级最高；本技能是把它落到操作上的步骤与本机的坑。两者冲突时以 `AGENTS.md` 为准。

## 0. 开工前

1. 读 `spec/README.md`、`spec/02_engineering_rules.md`；在 `spec/11_tasks.md` 找到卡片，确认「前置」都是 `DONE`。
2. 读卡片点名的规格章节与涉及的现有文件。不读完不写代码。
3. 不确定的行为不要猜：选最保守的做法，代码里写 `// SPEC-GAP(Txx): …`，交付说明里列出。

## 1. 起草规格（新卡或较大的改动）

写 `docs/handoff/Txx_<主题>.md`，结构沿用已有的卡（如 `T69_file_relay.md`、`T71_lan_sharing.md`）：

1. 标题注明「规格草案，待批准」；类型、前置、约束（不改 `internal/core`、`internal/protocol` 导出 API，不改分层规则与 adaptertest，不加依赖）。
2. 要解决的问题；配置示例；**规则表**（编号前缀 + 序号，如 `FS-01`），每条可测试。
3. 实现要点、改动文件、测试清单、实测（验收）步骤。
4. **待决策表**：问题 | 选项 | 建议（加粗选项 + 理由）。所有偏离 `AGENTS.md` 的地方（新包要在 archtest 登记、热路径磁盘 I/O、改测试期望值……）都必须作为决策项列出。
5. 在 `spec/11_tasks.md` 加卡片行（状态 `TODO`）。然后**停下等用户批准**。

用户回复「全部按建议」后：标题改为「已批准」，类型行加上批准日期。

## 2. 实现

- 只改卡片涉及的文件；需要动别处先说明理由。
- 规则：不忽略 error（`Close` / `Rollback` 可以 `_ =` 并加注释）；不用 `math/rand` 生成凭据；网络调用都有超时；`io.ReadAll` 都有上限；SQL 一律占位符；库代码不 `panic` / `os.Exit`；日志与错误中不出现 Key、Token 明文（用 `obs.MaskSecret`）。
- 管理 UI 的 JS 里不能出现 `http://` / `https://` 字面量、`innerHTML`、`localStorage`（`TestUIStaticRules`）；需要展示的 URL 由服务端拼好返回。新增文案中英文都要有（`i18n.js` 两处）。
- 每个新导出函数都有单元测试，每个分支至少一个用例；端到端测试放在 `cmd/gateway/e2e_*_test.go`（`startRESTGateway` 用内存配置；需要配置文件路径与重载时用 `startReloadGateway`）。
- 同步更新规格（spec/03 配置、spec/08 适配器、spec/09 HTTP 与 UI、spec/14 AI 接入……）、`docs/DEPLOYMENT.md`、`gateway.example.yaml`，以及相关技能（`.agents/skills/`）。

## 3. 完整检查

```bash
# Git Bash
export PATH="/c/Program Files/Go/bin:$PATH" GOFLAGS="-p=1" GOGC=25 GOMAXPROCS=2
go run ./tools/check
```

- 要几分钟：放到后台运行，把输出写进文件，结束后看最后几行；失败时用 `grep -nE "FAIL|_test\.go:[0-9]+:"` 找原因。**一定保存输出**，偶发失败才能定位。
- 本机没有 C 编译器，竞态检测会被跳过（输出里有 note），这是正常的；需要竞态检测时按 T61 / T46 在 Linux 上跑。
- 改了 `.agents/skills/` 后运行 `go run ./tools/skillsync` 同步 `.claude/skills/`，否则检查失败。
- `go build ./cmd/gateway` 会在仓库根目录留下 `gateway.exe`：用 `-o <scratch 目录>/gateway.exe`。

## 4. 实测

按 `live-test` 技能用真实上游验证验收步骤（只在本机、只输出计数、测完清理）。实测结果写进卡片文档的「实测结果」小节（勾选列表 + 关键数字），卡片标题改为「已批准，已完成」，`spec/11_tasks.md` 状态改为 `DONE`（这是唯一允许不经批准修改的 spec 内容）。

## 5. 交付说明

按 `AGENTS.md` §5 的格式：任务、改动文件、检查结果（最后几行）、规格缺口、未完成或有风险的地方。失败就如实说失败。

## 6. 提交（只在用户要求时）

- 一张卡一个提交：`feat(scope): Txx 说明`，类型取 feat / fix / test / docs / refactor / chore；结尾加上环境要求的 `Co-Authored-By` 行。
- 不 push，除非用户要求；不 amend、不 rebase、不 reset --hard。
- 提交前 `git status` 确认没有 `gateway.exe`、日志、`data/`、`secrets/`、账号文件。

**两张卡的改动混在工作区时拆成两个提交**（不改动工作区）：

1. 只属于第一张卡的文件直接 `git add`。
2. 两张卡都改过的文件：写一个脚本从工作区版本中去掉第二张卡的部分，输出到临时目录，再
   `h=$(git hash-object -w <临时文件>) && git update-index --cacheinfo 100644,$h,<路径>`。
3. 导出暂存区并单独检查：`git checkout-index -a --prefix=<临时目录>/`，在那里跑 `go run ./tools/check`。
   常见失败：切掉文件末尾的函数后多出一个空行 → 对临时文件 `gofmt -w` 后重新暂存。
4. 提交第一张卡；然后 `git add -A`，确认没有杂项文件，提交第二张卡。
