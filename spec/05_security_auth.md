# 05 安全与鉴权（Security & Auth）

代码位置：`internal/crypto`（已提供参考实现）、`internal/auth`（Token 与凭据提取已实现；索引重载待实现）、`internal/admin`（引导）。

## 1. 威胁模型（实现时以此为边界）

| 我们防护 | 我们**不**防护（写入文档，不要假装能防） |
|:--|:--|
| 数据库文件或其备份单独泄露：攻击者拿不到上游 Key 明文与客户端 Token | 攻击者能读取网关进程内存或以同一系统用户执行代码 |
| 数据库行被篡改（把 A 的密文挪给 B）：解密失败 | 攻击者同时拿到数据库与主密钥文件 |
| 客户端 Token 被暴力猜测：256 位随机，不可行 | 客户端自己泄露 Token（只能吊销） |
| 日志泄露凭据：所有凭据输出前脱敏 | 网络窃听：数据面对外暴露时必须由前置 TLS 终止（M1 不内置 TLS） |

## 2. 主密钥与派生（`internal/crypto`）

### 2.1 主密钥文件

`LoadOrCreateMasterKey(path string) ([]byte, error)`：

| 编号 | 行为 |
|:--|:--|
| SEC-01 | 文件不存在：创建父目录（`0700`），用 `crypto/rand` 生成 32 字节，以 `0600` 权限写入（先写临时文件再 `os.Rename`，防止写一半） |
| SEC-02 | 文件存在：读取，长度必须恰好 32 字节，否则返回错误（不得截断或补齐） |
| SEC-03 | 返回的切片是调用方独占的副本 |

### 2.2 子密钥派生

使用标准库 `crypto/hkdf`（SHA-256，salt 为空）。`info` 字符串是协议常量，**永远不能修改**：

| 用途 | info | 长度 |
|:--|:--|:--|
| 包裹 DEK 的 KEK | `uaig/v1/kek` | 32 |
| Token / 指纹 HMAC 的 pepper | `uaig/v1/token-pepper` | 32 |

### 2.3 信封加密（上游 Key）

```go
type Sealed struct {
    KEKVersion int
    WrappedDEK []byte // nonce(12) || AES-256-GCM(KEK, DEK, aad) || tag(16)
    Ciphertext []byte // nonce(12) || AES-256-GCM(DEK, plaintext, aad) || tag(16)
}
func (k *Keyring) Seal(plaintext, aad []byte) (Sealed, error)
func (k *Keyring) Open(s Sealed, aad []byte) ([]byte, error)
func UpstreamKeyAAD(id int64, pool string, kekVersion int) []byte
```

| 编号 | 行为 |
|:--|:--|
| SEC-04 | `Seal`：每次生成新的随机 32 字节 DEK 与两个独立随机 nonce；同一明文两次 `Seal` 结果不同 |
| SEC-05 | `Open`：`s.KEKVersion` 与 Keyring 版本不一致返回 `ErrUnknownKEKVersion` |
| SEC-06 | `Open`：AAD 不一致、密文被改动任一字节，都返回 `ErrDecrypt`（不暴露具体原因） |
| SEC-07 | AAD 格式固定为 `uaig:v1:upstream_keys:<id>:<pool>:<kek_version>`，由 `UpstreamKeyAAD` 生成，其他地方**禁止**手写 AAD |

## 3. 客户端 Token（`internal/auth`）

### 3.1 格式

- 用户 Token：`uai_live_` + base64url（无填充）编码的 32 字节随机数（共 52 字符）。
- 管理员 Token：同样的格式（角色存在数据库里，不体现在前缀中，避免前缀泄露权限信息）。
- 展示串 `token_display`：前 15 个字符 + `…`，例如 `uai_live_ab12cd…`。

### 3.2 已提供的函数

```go
func GenerateToken() (plain, display string, err error)
func HashToken(pepper []byte, plain string) string        // hex(HMAC-SHA256(pepper, plain))
func ExtractCredential(h http.Header) (string, bool)
```

`ExtractCredential` 规则（AUTH-01 ~ AUTH-04）：

| 编号 | 规则 |
|:--|:--|
| AUTH-01 | 依次检查 `Authorization`（必须以 `Bearer ` 开头，大小写不敏感，取其后内容并去掉首尾空格）、`x-api-key`、`x-goog-api-key`，返回第一个非空值 |
| AUTH-02 | 取到的值必须以 `uai_live_` 开头，否则视为没有凭据（返回 `"", false`）。这样客户端误把上游 Key 发给网关时，不会被拿去做 HMAC 查找 |
| AUTH-03 | 不读取 URL 查询参数（避免 Token 出现在访问日志里） |
| AUTH-04 | 值长度超过 256 字节视为无效 |

### 3.3 内存索引

```go
type Principal struct {
    ClientID      int64
    Name          string
    Role          string   // "user" | "admin"
    RPMLimit      int
    AllowedModels []string // 空 = 全部允许
    Disabled      bool
}
type Index struct{ /* atomic.Pointer[map[string]*Principal] */ }
func NewIndex() *Index
func (ix *Index) Lookup(tokenHash string) (*Principal, bool)
func (ix *Index) Replace(clients []store.Client)
func (p *Principal) ModelAllowed(model string) bool
```

| 编号 | 行为 |
|:--|:--|
| AUTH-05 | `Lookup` 无锁、O(1)，并发安全 |
| AUTH-06 | `Replace` 构建全新的 map 后用 `atomic.Pointer.Store` 一次性替换；进行中的 `Lookup` 要么看到旧快照要么看到新快照，不会看到一半 |
| AUTH-07 | `Replace` 包含被禁用的客户端（以便返回 403 而不是 401） |
| AUTH-08 | 返回的 `*Principal` 只读；调用方**禁止**修改它的字段 |
| AUTH-09 | `ModelAllowed`：`AllowedModels` 为空返回 true；否则精确匹配（区分大小写） |

### 3.4 索引重载（任务 T03）

`auth.Reloader{Store ClientLister; Index *Index}`：`ClientLister` 是只含 `ListClients` 的小接口（`store.Store` 自然满足它），方法 `Reload(ctx) error` 调用 `ListClients` 并 `Replace`。
管理 API 每次修改客户端后**同步**调用 `Reload`（管理操作低频，可接受）。

## 4. 管理面鉴权（`internal/admin`）

| 编号 | 行为 |
|:--|:--|
| ADM-01 | 管理面所有 `/admin/api/*` 端点都要求凭据，提取方式同 AUTH-01/02 |
| ADM-02 | 凭据对应的 Principal 必须 `Role == "admin"` 且未禁用，否则：无凭据/未知 → 401；非 admin → 403 |
| ADM-03 | 管理员不能禁用或降级**最后一个**未禁用的管理员（`CountAdmins` 检查），返回 409 `conflict` |
| ADM-04 | 同一来源 IP 在 1 分钟内鉴权失败 ≥ 20 次后，该 IP 后续请求直接返回 429（内存计数，不 sleep） |

## 5. 管理员引导

| 编号 | 行为 |
|:--|:--|
| BOOT-01 | 启动时 `CountAdmins == 0`：生成 Token，创建 `name="bootstrap-admin"`、`role="admin"`、`quota_unlimited=1`、`rpm_limit=0` 的客户端 |
| BOOT-02 | 把明文 Token 写到 **stderr** 一次，格式：`BOOTSTRAP ADMIN TOKEN (shown once): uai_live_...`。这是全系统唯一允许输出 Token 明文的地方，不经过 slog（直接 `fmt.Fprintln(os.Stderr, ...)`，仅在 `cmd/gateway` 中） |
| BOOT-03 | 已存在管理员时不做任何事 |

## 6. 脱敏（`obs.MaskSecret`，已提供）

- 长度 ≤ 8：返回 `****`。
- 否则：前 4 个字符 + `****` + 后 4 个字符。
- 用于日志、`secret_hint` 列、管理 API 返回。
