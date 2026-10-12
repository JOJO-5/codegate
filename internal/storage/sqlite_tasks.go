package storage

import (
	"context"
	"encoding/json"
	"github.com/jojo/codegate/internal/protocol"
)

func (s *SQLite) InsertContinuation(ctx context.Context, t *protocol.ContinuationTask, nonce string) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO continuation_tasks(id,user_id,device_id,nonce,state,due_at,payload_json) VALUES(?,?,?,?,?,?,?)", t.ID, t.UserID, t.DeviceID, nonce, t.State, t.DueAt, string(b))
	return wrapErr(err)
}
func (s *SQLite) ContinuationByID(ctx context.Context, id string) (*protocol.ContinuationTask, error) {
	var body, user string
	err := s.db.QueryRowContext(ctx, "SELECT payload_json,user_id FROM continuation_tasks WHERE id=?", id).Scan(&body, &user)
	if err != nil {
		return nil, wrapErr(err)
	}
	var t protocol.ContinuationTask
	if err = json.Unmarshal([]byte(body), &t); err != nil {
		return nil, err
	}
	t.UserID = user
	return &t, nil
}
func (s *SQLite) ContinuationByNonce(ctx context.Context, user, nonce string) (*protocol.ContinuationTask, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM continuation_tasks WHERE user_id=? AND nonce=?", user, nonce).Scan(&id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return s.ContinuationByID(ctx, id)
}
func (s *SQLite) Continuations(ctx context.Context, user, device string) ([]*protocol.ContinuationTask, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT payload_json,user_id FROM continuation_tasks WHERE (?='' OR user_id=?) AND (?='' OR device_id=?) ORDER BY json_extract(payload_json,'$.created_at') DESC,id LIMIT 200", user, user, device, device)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	out := []*protocol.ContinuationTask{}
	for rows.Next() {
		var body, owner string
		if err = rows.Scan(&body, &owner); err != nil {
			return nil, err
		}
		var t protocol.ContinuationTask
		if err = json.Unmarshal([]byte(body), &t); err != nil {
			return nil, err
		}
		t.UserID = owner
		out = append(out, &t)
	}
	return out, rows.Err()
}

// Expected-state compare prevents cancellation from being overwritten by a late
// worker result or two scheduler ticks from claiming the same queued attempt.
func (s *SQLite) UpdateContinuation(ctx context.Context, t *protocol.ContinuationTask, expected string) (bool, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE continuation_tasks SET state=?,due_at=?,payload_json=? WHERE id=? AND user_id=? AND state=?", t.State, t.DueAt, string(b), t.ID, t.UserID, expected)
	if err != nil {
		return false, wrapErr(err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *SQLite) ReadyContinuations(ctx context.Context, now int64) ([]*protocol.ContinuationTask, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT payload_json,user_id FROM continuation_tasks WHERE state IN ('queued','waiting_agent','waiting_quota','waiting_idle','dispatching','running','cancelling') AND due_at<=? ORDER BY due_at,id LIMIT 32", now)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	out := []*protocol.ContinuationTask{}
	for rows.Next() {
		var body, user string
		if err = rows.Scan(&body, &user); err != nil {
			return nil, err
		}
		var t protocol.ContinuationTask
		if err = json.Unmarshal([]byte(body), &t); err != nil {
			return nil, err
		}
		t.UserID = user
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *SQLite) ActiveContinuationCount(ctx context.Context, user, device string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM continuation_tasks WHERE user_id=? AND device_id=? AND state NOT IN ('completed','cancelled','failed','needs_review')", user, device).Scan(&count)
	return count, wrapErr(err)
}
