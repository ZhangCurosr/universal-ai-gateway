-- T63：按 Key 与时间重算周期额度用量（spec/04 STO-20、STO-21）。
CREATE INDEX idx_usage_key_time ON usage_events(key_id, created_at);
