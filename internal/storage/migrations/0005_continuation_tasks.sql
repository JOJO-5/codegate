CREATE TABLE continuation_tasks (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
 nonce TEXT NOT NULL,
 state TEXT NOT NULL,
 due_at INTEGER NOT NULL,
 payload_json TEXT NOT NULL,
 UNIQUE(user_id, nonce)
);
CREATE INDEX continuation_due ON continuation_tasks(state, due_at);
CREATE INDEX continuation_owner ON continuation_tasks(user_id, device_id);
