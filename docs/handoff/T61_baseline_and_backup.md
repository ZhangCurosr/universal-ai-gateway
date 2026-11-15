# T61 基线验证、离线备份与恢复（已批准，已完成）

版本：2026-11-13。用户已批准建议方案及文件范围（包括 256 MiB 上限、恢复后手动调整配置路径）。状态：DONE（2026-11-14 验收）。前置：无。必读：AGENTS.md、spec/02、spec/03、spec/04、spec/12、spec/15、T46 验证步骤，以及下列涉及文件。

## 目标与约束

提供 gateway backup / restore，在网关离线时创建一致、可验证的备份并恢复到新目录。只使用标准库和已有 SQLite 驱动；不改 core / protocol 的导出 API，不新增依赖或分层规则。备份失败不破坏来源，恢复失败不覆盖现有实例。

## 已批准决策

1. 只做离线备份：命令先获取现有 sqlite.AcquireDirLock 的 gateway.lock；网关运行时拒绝备份或恢复。持锁至命令结束。运维方停止网关后再执行。
2. 默认备份文件包含数据库、gateway.yaml、主密钥与未刷盘计费文件（若有）；因为数据库与主密钥需要配套才能完整迁移，整个归档必须加密。主密钥不能在归档外再次以明文导出。
3. 口令只通过 UAIG_BACKUP_PASSWORD 环境变量传入，长度至少 16 字节；没有命令行口令参数，不打印口令、Token、主密钥或数据库内容。首版不增加交互式隐藏输入，避免引入终端依赖。
4. 归档为标准库 archive/tar 的固定白名单成员，再由 AES-256-GCM 认证加密。口令派生使用标准库 crypto/pbkdf2（HMAC-SHA256，600000 次）、随机 16 字节 salt、随机 GCM nonce。格式有固定 magic、版本、KDF 参数和明文长度；整个头作为 AAD。解密前严格校验格式、KDF 和长度上限，避免攻击者放大工作量。无压缩，避免解压炸弹。
5. 首版总负载上限 256 MiB（显式 --max-bytes 可降低，不能提高）；实现采用有界整包 AEAD，内存预算和最大支持库大小写入文档。更大数据库的分块认证格式另立任务，不能用未经认证的流式输出替代。该限制已由用户批准。
6. SQLite 一致性：持目录锁，使用 database/sql 与已有驱动（cmd/gateway 通过 internal/store/sqlite 的现有导入注册驱动）以只读模式打开现有库；检查 schema 不高于当前版本，执行参数化 VACUUM INTO ? 写到私有临时数据库，再将完整快照加入归档。先用带 WAL 的合成实例验证此路径，不调用会迁移来源的 sqlite.Open，不对来源做 checkpoint。archive 中不带 gateway.lock、-wal、-shm 或任意数据目录文件。
7. 归档清单 manifest.json 写格式版本、数据库 schema、各成员大小和 SHA-256；不写原主密钥路径、上游 URL 或凭据。只允许 manifest.json、gateway.db、gateway.yaml、master.key、unflushed_usage.jsonl 五个固定文件名。禁止重复条目、目录、符号链接、硬链接、路径穿越和超限成员。
8. 备份输出默认拒绝覆盖。临时文件位于输出同目录，权限 0600；写入、Sync、Close 全部检查成功后发布。发布必须用不覆盖已有文件的方式（不以 Unix Rename 的覆盖语义冒充拒绝覆盖）。任一失败清理临时文件并返回错误。
9. 恢复只接受不存在的新目标目录及新的配置/主密钥路径，不支持 --force。父目录必须已经存在且不能含符号链接。先完整验证 GCM、manifest、校验和、SQLite integrity_check、schema 和归档中的主密钥格式，再在目标同文件系统的临时目录解包。数据、配置、密钥都验完才发布；多文件发布失败需要回滚本命令新建的文件，不触碰既有文件。恢复期间获取目标旁的互斥锁，避免同一目标并发恢复。
10. 配置恢复：原配置按字节保留，用户手工调整 server.data_dir / master_key_file 后运行 check-config。不增加配置编辑 API，不自动改写 YAML（已批准的首版取舍）。
11. 恢复不启动网关、不发送上游请求；已有客户端 Token、Key、计费历史、运行时状态全部随库保留。已有客户端 Token 无法从备份重新显示，管理员需要持有原 Token。
12. Windows 的权限语义与目录发布行为需要独立验证。Go 可交叉构建，但不能把 Linux 文件权限测试当作 Windows 已验证。

## 拟定命令

```
gateway backup --config gateway.yaml --output backup.uaig [--max-bytes 268435456]
gateway restore --file backup.uaig --data-dir ./restored/data --master-key-file ./restored/secrets/master.key --config-output ./restored/gateway.yaml
```

restore 的配置为原配置副本，输出明确提示手动调整 data_dir / master_key_file；调整后运行 check-config。

## 拟修改文件（需随方案一并授权）

- spec/03_config.md：命令、退出码与资源上限。
- spec/04_storage.md：离线一致性与恢复语义。
- spec/12_deployment.md：离线备份与恢复步骤、口令保管、大小与平台限制。
- spec/11_tasks.md：T61 涉及文件及状态。
- cmd/gateway/main.go：注册子命令和用法。
- cmd/gateway/backup.go（新增）：离线锁、数据库一致性快照、归档和认证加密。
- cmd/gateway/restore.go（新增）：验证、暂存、发布和失败回滚。
- cmd/gateway/backup_test.go、restore_test.go（新增）：测试。
- docs/DEPLOYMENT.md：操作说明与恢复演练。
- docs/handoff/T61_baseline_and_backup.md（新增）：正式任务卡及验收结果。
- docs/handoff/reports/T61_baseline_report.md（新增）：竞态及稳定性报告。
- tools/soak/（新增，可选）：将 /tmp 中经短测验证的外部负载驱动整理为可复跑工具。无需业务代码变更。

## 最低验收

- 用包含客户端、加密 Key、计费事件、运行时状态及 unflushed_usage.jsonl 的实例备份到新位置；恢复后 Token 可鉴权、Key 可解密、账目与固定记录一致。
- 错误口令、篡改、截断、未知格式、schema 过新、成员重复、缺失成员、哈希错误、路径穿越、符号链接、超限归档均拒绝且无部分发布。
- 网关在线、输出已存在、目标有数据、路径冲突、源文件是符号链接、磁盘写入失败、并发恢复均拒绝或正确回滚。
- 所有输出和错误不出现测试用口令、Token、主密钥及上游 Key。
- 检查归档白名单；源数据和主密钥文件前后校验和不变。
- 完整检查含 race、零 CGO 构建和分层测试通过；Windows 未实测项明确标注。

## 审批记录与当前进度

2026-11-13 用户回复「批准建议方案与文件范围」。T55 既有竞态修复由先前的「修复」授权，保留在本工作区，作为本卡验证基线的必要前置修复。

- [x] 带已有修复的固定快照完整检查、两组 Linux race 各 30 次通过。
- [x] 60 秒稳定性驱动短测通过。
- [x] 24 小时稳定性基线完成：主动负载 86400.529 秒，172788 次请求 / 172788 条计费事件，0 错误，287 次热重载；数据库完整性为 ok。
- [x] 备份恢复完整实现、负面测试与恢复演练、最终完整检查（含 race、零 CGO、架构检查）通过。
- [x] 报告与部署说明已交付；Windows 正式 gateway.exe 已做本地 CLI 备份恢复演练，包含 schema 4、拒绝覆盖和错口令；ACL 与符号链接完整语义未验证。报告见 [T61_baseline_report.md](reports/T61_baseline_report.md)。
- [x] 整卡验收满足，标为 DONE。

正式规则见 spec/03 §9、spec/04 §5、spec/12 §8。

## 2026-11-14 收尾与补充授权

- git pull 后基于 `2267732` 恢复已批准的 T61 实现（原提交 `08d7d07` 尚未进入 main），保留当前工作区已有的 MCP / YouTube 与调度器改动。
- 已有数据库迁移 `0004_usage_key_index.sql`：备份支持上限更新为 schema 4，版本守卫测试仍与实际迁移数比对；历史 schema 3 固定断言改为当前版本，未放宽数据完整性要求。
- 修复 Windows 盘符 URI：`D:` 作为文件路径而不是 URI authority。已由正式 Windows gateway.exe 的实际备份 / 恢复验证。
- 用户于 2026-11-14 批准卡外最小修复：`cmd/gateway/e2e_rest_api_test.go` 的重置时刻使用当前时间之后 12 小时，保留「两把 Key 冷却超过 1 小时」的原断言；该测试 30 次 race 回归通过。此文件纳入本次授权范围。
- 全天负载与两组各 30 次 race 使用固定版本 `71f32a3` + T55 补丁；当前 `2267732` 工作区另经完整检查与重启验证。完成历史基线不等于宣称最新所有功能都已跑过全天负载。
- 已重新编译根目录 gateway.exe，并生成相同源码的 Linux 运行文件；当前 8081 实例实际运行于 WSL，已优雅重启，登录、页面、配置编辑资源与数据保留均核对通过。
- 完整 Windows 测试程序执行受应用程序控制策略限制，没有记录为通过；正式 gateway.exe 的版本检查和合成数据 CLI 演练已实际通过。

## 2026-11-15 提交整理

- 配合云端 T79 的第 5 个数据库迁移，将备份支持上限更新为 5；版本守卫与既有备份/恢复测试保持启用，完整检查通过。
- 按用户「每次做完记得提交推送 git」的要求，将已完成的 T61 与 T80 分别提交；保留未完成的本地 MCP / YouTube 和调度器工作。
