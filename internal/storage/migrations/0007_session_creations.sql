-- Reserve before dispatch. An unresolved intent is never automatically replayed.
CREATE TABLE session_creations (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
 nonce TEXT NOT NULL,
 request_id TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 result_json TEXT,
 created_at INTEGER NOT NULL,
 PRIMARY KEY(user_id,device_id,nonce),
 UNIQUE(user_id,device_id,request_id)
);
