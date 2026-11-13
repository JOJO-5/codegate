package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/storage"
)

func creationFingerprint(req protocol.SessionCreatePayload) string {
	req.Nonce = ""
	req.Recovery = nil
	// Attaching will negotiate viewport size; it does not change the intention.
	req.Cols, req.Rows = 0, 0
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Return true when the intent was handled; never dispatch a pending reservation.
func (s *clientSession) existingCreation(env *protocol.Envelope, device string, reserve bool) bool {
	if env.Type != protocol.TypeSessionCreate {
		return false
	}
	req, err := protocol.DecodePayload[protocol.SessionCreatePayload](env)
	if err != nil {
		sendErrorEnvelope(s.c.TrySendText, env, err)
		return true
	}
	if req.Nonce == "" {
		return false
	} // Older clients remain compatible.
	if _, err = uuid.Parse(req.Nonce); err != nil {
		sendErrorEnvelope(s.c.TrySendText, env, protocol.ErrInvalidPayload)
		return true
	}
	ctx, cancel := s.dbCtx()
	defer cancel()
	if reserve {
		inserted, e := s.srv.store.ReserveSessionCreation(ctx, &storage.SessionCreation{UserID: s.c.UserID, DeviceID: device, Nonce: req.Nonce, RequestID: env.RequestID, Fingerprint: creationFingerprint(req), CreatedAt: s.srv.now().UnixMilli()})
		if e != nil {
			sendErrorEnvelope(s.c.TrySendText, env, e)
			return true
		}
		if inserted {
			return false
		}
	}
	c, err := s.srv.store.SessionCreation(ctx, s.c.UserID, device, req.Nonce)
	if errors.Is(err, storage.ErrNotFound) && !reserve {
		return false
	}
	if err != nil {
		sendErrorEnvelope(s.c.TrySendText, env, err)
		return true
	}
	if c.Fingerprint != creationFingerprint(req) {
		sendErrorEnvelope(s.c.TrySendText, env, protocol.NewError(protocol.CodeInvalidPayload, "启动标识已用于其他参数，请核对原启动结果"))
	} else if c.Result == nil {
		sendErrorEnvelope(s.c.TrySendText, env, protocol.NewError(protocol.CodeRequestOutcomeUnknown, "启动已派发，结果待确认；请核对启动结果，不会重复创建"))
	} else {
		var payload any
		_ = json.Unmarshal(c.Result.Payload, &payload)
		s.sendEnvelope(c.Result.Type, payload, env)
	}
	return true
}

func (s *Server) handleSessionCreation(w http.ResponseWriter, r *http.Request) {
	user, _ := userIDFromContext(r.Context())
	device := r.PathValue("id")
	if _, err := s.authorizeDevice(r.Context(), user, device); err != nil {
		writeProtoError(w, err)
		return
	}
	c, err := s.store.SessionCreation(r.Context(), user, device, r.PathValue("nonce"))
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, 200, map[string]any{"state": "not_received"})
		return
	}
	if err != nil {
		writeProtoError(w, err)
		return
	}
	state := "pending"
	if c.Result != nil {
		state = "resolved"
	}
	writeJSON(w, 200, map[string]any{"state": state, "result": c.Result})
}
