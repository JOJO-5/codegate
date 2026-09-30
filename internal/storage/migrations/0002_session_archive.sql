ALTER TABLE sessions ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_sess_archived ON sessions(device_id, archived, created_at DESC);
