CREATE TABLE workflows (
 id TEXT PRIMARY KEY,
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
 nonce TEXT NOT NULL,
 state TEXT NOT NULL,
 updated_at INTEGER NOT NULL,
 payload_json TEXT NOT NULL,
 UNIQUE(user_id,nonce)
);
CREATE INDEX workflows_device_state ON workflows(device_id,state,updated_at);
