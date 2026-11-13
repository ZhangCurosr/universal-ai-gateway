-- T68：调度器运行时状态的快照（spec/04 STO-18、STO-19，spec/07 RT 规则）。由控制面整体替换，数据面不访问。
CREATE TABLE key_runtime (
    key_id         INTEGER PRIMARY KEY,
    cooldown_until INTEGER NOT NULL,   -- Unix 毫秒；0 = 没有冷却
    auth_failures  INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
CREATE TABLE task_pins (
    affinity_key TEXT    PRIMARY KEY,  -- 内核算出的亲和键（哈希），不含任务 ID 明文
    key_id       INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL      -- Unix 毫秒
);
