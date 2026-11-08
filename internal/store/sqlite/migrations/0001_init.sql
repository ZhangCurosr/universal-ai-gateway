-- 0001_init：初始表结构（spec/04 §3）。已发布的迁移禁止修改，改结构请新增 0002_xxx.sql。

CREATE TABLE clients (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL,
    role            TEXT    NOT NULL CHECK (role IN ('user', 'admin')),
    token_hash      TEXT    NOT NULL UNIQUE,
    token_display   TEXT    NOT NULL,
    quota_unlimited INTEGER NOT NULL DEFAULT 0 CHECK (quota_unlimited IN (0, 1)),
    quota_nano_usd  INTEGER NOT NULL DEFAULT 0 CHECK (quota_nano_usd >= 0),
    rpm_limit       INTEGER NOT NULL DEFAULT 60 CHECK (rpm_limit >= 0),
    allowed_models  TEXT    NOT NULL DEFAULT '[]',
    disabled        INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE upstream_keys (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    pool          TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    fingerprint   TEXT    NOT NULL UNIQUE,
    secret_hint   TEXT    NOT NULL,
    kek_version   INTEGER NOT NULL,
    wrapped_dek   BLOB    NOT NULL,
    ciphertext    BLOB    NOT NULL,
    weight        INTEGER NOT NULL DEFAULT 1 CHECK (weight >= 1),
    max_in_flight INTEGER NOT NULL DEFAULT 16 CHECK (max_in_flight >= 1),
    state         TEXT    NOT NULL DEFAULT 'ready' CHECK (state IN ('ready', 'quarantined', 'disabled')),
    state_reason  TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX idx_upstream_keys_pool ON upstream_keys(pool);

CREATE TABLE usage_events (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id         TEXT    NOT NULL UNIQUE,
    client_id          INTEGER NOT NULL,
    key_id             INTEGER NOT NULL,
    model              TEXT    NOT NULL,
    op                 TEXT    NOT NULL,
    usage              TEXT    NOT NULL,          -- JSON 对象：计量单位 → 数量，键按字母序
    cost_nano_usd      INTEGER NOT NULL,
    usage_source       TEXT    NOT NULL CHECK (usage_source IN ('upstream', 'estimated')),
    created_at         INTEGER NOT NULL
);
CREATE INDEX idx_usage_client_time ON usage_events(client_id, created_at);

CREATE TABLE audit_records (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id        TEXT    NOT NULL,
    client_id         INTEGER NOT NULL,
    key_id            INTEGER NOT NULL,
    model             TEXT    NOT NULL,
    op                TEXT    NOT NULL,
    ingress_protocol  TEXT    NOT NULL,
    upstream_protocol TEXT    NOT NULL,
    status            INTEGER NOT NULL,
    error_code        TEXT    NOT NULL,
    outcome           TEXT    NOT NULL,
    attempts          INTEGER NOT NULL,
    stream            INTEGER NOT NULL,
    ttfb_ms           INTEGER NOT NULL,
    latency_ms        INTEGER NOT NULL,
    created_at        INTEGER NOT NULL
);
CREATE INDEX idx_audit_time ON audit_records(created_at);

CREATE TABLE system_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
