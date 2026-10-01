package storage

import "context"

// Preferences are written one key at a time so independent browser changes
// cannot replace the entire account's project and pin collection.
func (s *SQLite) Preferences(ctx context.Context, userID string) (map[string]string, error) {
    rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM user_preferences WHERE user_id = ?`, userID)
    if err != nil { return nil, err }
    defer rows.Close()
    out := make(map[string]string)
    for rows.Next() {
        var key, value string
        if err := rows.Scan(&key, &value); err != nil { return nil, err }
        out[key] = value
    }
    return out, rows.Err()
}

func (s *SQLite) SetPreference(ctx context.Context, userID, key, value string) error {
    _, err := s.db.ExecContext(ctx, `INSERT INTO user_preferences(user_id, key, value) VALUES (?, ?, ?)
        ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`, userID, key, value)
    return err
}

func (s *SQLite) DeletePreference(ctx context.Context, userID, key string) error {
    _, err := s.db.ExecContext(ctx, `DELETE FROM user_preferences WHERE user_id = ? AND key = ?`, userID, key)
    return err
}
