-- T79：全体客户端按时间统计用量（spec/04 STO-23）；现有索引都以 client_id / key_id 开头。
CREATE INDEX idx_usage_time ON usage_events(created_at);
