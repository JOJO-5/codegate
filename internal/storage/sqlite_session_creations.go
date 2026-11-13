package storage

import (
	"context"
	"encoding/json"

	"github.com/jojo/codegate/internal/protocol"
)

type SessionCreation struct {
	UserID, DeviceID, Nonce, RequestID, Fingerprint string
	CreatedAt                                       int64
	Result                                          *protocol.Envelope
}

func (s *SQLite) ReserveSessionCreation(ctx context.Context, c *SessionCreation) (bool, error) {
	r, err := s.db.ExecContext(ctx, "INSERT INTO session_creations(user_id,device_id,nonce,request_id,fingerprint,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING", c.UserID, c.DeviceID, c.Nonce, c.RequestID, c.Fingerprint, c.CreatedAt)
	if err != nil {
		return false, wrapErr(err)
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *SQLite) SessionCreation(ctx context.Context, user, device, nonce string) (*SessionCreation, error) {
	c := &SessionCreation{UserID: user, DeviceID: device, Nonce: nonce}
	var result []byte
	err := s.db.QueryRowContext(ctx, "SELECT request_id,fingerprint,created_at,result_json FROM session_creations WHERE user_id=? AND device_id=? AND nonce=?", user, device, nonce).Scan(&c.RequestID, &c.Fingerprint, &c.CreatedAt, &result)
	if err != nil {
		return nil, wrapErr(err)
	}
	if result != nil {
		c.Result = &protocol.Envelope{}
		if err = json.Unmarshal(result, c.Result); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func (s *SQLite) ResolveSessionCreation(ctx context.Context, user, device, request string, result *protocol.Envelope) error {
	// Persist only the receipt; transport identities are replaced for each caller.
	b, err := json.Marshal(&protocol.Envelope{Type: result.Type, Payload: result.Payload})
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE session_creations SET result_json=? WHERE user_id=? AND device_id=? AND request_id=? AND result_json IS NULL", string(b), user, device, request)
	return wrapErr(err)
}
