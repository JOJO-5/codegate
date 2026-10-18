package storage

import (
	"context"
	"encoding/json"
	"github.com/jojo/codegate/internal/protocol"
)

func (s *SQLite) InsertWorkflow(ctx context.Context, w *protocol.Workflow, nonce string) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO workflows(id,user_id,device_id,nonce,state,updated_at,payload_json) VALUES(?,?,?,?,?,?,?)", w.ID, w.UserID, w.DeviceID, nonce, w.State, w.UpdatedAt, string(b))
	return wrapErr(err)
}
func (s *SQLite) Workflow(ctx context.Context, id string) (*protocol.Workflow, error) {
	var body, owner string
	err := s.db.QueryRowContext(ctx, "SELECT payload_json,user_id FROM workflows WHERE id=?", id).Scan(&body, &owner)
	if err != nil {
		return nil, wrapErr(err)
	}
	var w protocol.Workflow
	err = json.Unmarshal([]byte(body), &w)
	w.UserID = owner
	return &w, err
}
func (s *SQLite) WorkflowByNonce(ctx context.Context, user, nonce string) (*protocol.Workflow, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM workflows WHERE user_id=? AND nonce=?", user, nonce).Scan(&id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return s.Workflow(ctx, id)
}
func (s *SQLite) Workflows(ctx context.Context, user, device string) ([]*protocol.Workflow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT payload_json,user_id FROM workflows WHERE (?='' OR user_id=?) AND (?='' OR device_id=?) ORDER BY CASE WHEN state IN ('ready_for_review','cancelled','failed','needs_review') THEN 1 ELSE 0 END,updated_at DESC,id LIMIT 100", user, user, device, device)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	result := []*protocol.Workflow{}
	for rows.Next() {
		var body, owner string
		if err = rows.Scan(&body, &owner); err != nil {
			return nil, err
		}
		var w protocol.Workflow
		if err = json.Unmarshal([]byte(body), &w); err != nil {
			return nil, err
		}
		w.UserID = owner
		result = append(result, &w)
	}
	return result, rows.Err()
}
func (s *SQLite) UpdateWorkflow(ctx context.Context, w *protocol.Workflow, expected int64) (bool, error) {
	b, err := json.Marshal(w)
	if err != nil {
		return false, err
	}
	r, err := s.db.ExecContext(ctx, "UPDATE workflows SET state=?,updated_at=?,payload_json=? WHERE id=? AND user_id=? AND json_extract(payload_json,'$.revision')=?", w.State, w.UpdatedAt, string(b), w.ID, w.UserID, expected)
	if err != nil {
		return false, wrapErr(err)
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
