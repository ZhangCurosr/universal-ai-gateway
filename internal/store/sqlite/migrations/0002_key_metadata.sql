-- T62：上游 Key 的过期时间与来源标签（spec/04）。只加列，旧数据不变。
ALTER TABLE upstream_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0; -- Unix 秒；0 = 不过期
ALTER TABLE upstream_keys ADD COLUMN label      TEXT    NOT NULL DEFAULT '';
