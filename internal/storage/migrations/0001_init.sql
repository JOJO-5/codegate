-- CodeGate 初始 schema（架构文档 §13.1）
--
-- 约定：
--   * 时间统一存 Unix 毫秒整数（INTEGER），不用 DATETIME/TIMESTAMP。
--     SQLite 没有原生时间类型、PG 有，用整数可在两库之间零转换（§13.2）。
--   * 布尔用 INTEGER 0/1（PG 对应 boolean）。
--   * id 一律 UUIDv7 文本。
--   * 索引只建在真实查询路径上：按 email 查用户、按 user 查设备、
--     按 device 查活跃 session、按 user+时间查审计。其余不建。

-- ============ users ============
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user',
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_users_email ON users(lower(email));

-- ============ refresh_tokens ============
CREATE TABLE refresh_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  BLOB NOT NULL,
    family_id   TEXT NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,
    revoked_at  INTEGER,
    user_agent  TEXT,
    ip          TEXT,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_rt_user   ON refresh_tokens(user_id);
CREATE INDEX idx_rt_family ON refresh_tokens(family_id);
CREATE INDEX idx_rt_exp    ON refresh_tokens(expires_at);
-- 令牌哈希是查询入口（每次刷新都按它查），必须唯一且带索引。
CREATE UNIQUE INDEX idx_rt_hash ON refresh_tokens(token_hash);

-- ============ devices ============
CREATE TABLE devices (
    id            TEXT PRIMARY KEY,
    user_id       TEXT REFERENCES users(id) ON DELETE SET NULL,
    name          TEXT NOT NULL,
    platform      TEXT NOT NULL,
    arch          TEXT NOT NULL,
    agent_version TEXT NOT NULL,
    public_key    BLOB NOT NULL,
    last_seen_at  INTEGER,
    paired_at     INTEGER,
    revoked_at    INTEGER,
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_dev_user ON devices(user_id);

-- ============ pairing_codes ============
CREATE TABLE pairing_codes (
    id            TEXT PRIMARY KEY,
    code_hash     BLOB NOT NULL,
    device_id     TEXT NOT NULL,
    public_key    BLOB NOT NULL,
    name          TEXT NOT NULL,
    platform      TEXT NOT NULL,
    arch          TEXT NOT NULL,
    agent_version TEXT NOT NULL,
    agent_ip      TEXT,
    expires_at    INTEGER NOT NULL,
    used_at       INTEGER,
    used_by       TEXT REFERENCES users(id),
    created_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_pc_hash ON pairing_codes(code_hash);
CREATE INDEX idx_pc_exp ON pairing_codes(expires_at);

-- ============ sessions（Server 侧元数据缓存，非真相源）============
CREATE TABLE sessions (
    id               TEXT PRIMARY KEY,
    device_id        TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    command          TEXT NOT NULL,
    args_json        TEXT NOT NULL DEFAULT '[]',
    cwd              TEXT NOT NULL,
    status           TEXT NOT NULL,
    pid              INTEGER,
    exit_code        INTEGER,
    cols             INTEGER NOT NULL,
    rows             INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    started_at       INTEGER,
    ended_at         INTEGER,
    last_attached_at INTEGER,
    created_by       TEXT REFERENCES users(id)
);
CREATE INDEX idx_sess_device ON sessions(device_id, status);
CREATE INDEX idx_sess_user   ON sessions(user_id, created_at DESC);

-- ============ audit_logs ============
CREATE TABLE audit_logs (
    id         TEXT PRIMARY KEY,
    user_id    TEXT,
    device_id  TEXT,
    session_id TEXT,
    action     TEXT NOT NULL,
    result     TEXT NOT NULL,
    ip         TEXT,
    user_agent TEXT,
    meta_json  TEXT,
    created_at INTEGER NOT NULL
);
CREATE INDEX idx_audit_user ON audit_logs(user_id, created_at DESC);
CREATE INDEX idx_audit_act  ON audit_logs(action, created_at DESC);
