package server

import (
    "context"
    "net/http"
    "testing"

    "github.com/google/uuid"
    "github.com/jojo/codegate/internal/storage"
)

func TestPreferencesAccountIsolation(t *testing.T) {
    env := newTestEnv(t)
    alice := env.signup(t, "alice-prefs@example.com", "correct-horse-battery")
    bob := env.signup(t, "bob-prefs@example.com", "correct-horse-battery")
    aliceID := env.meUserID(t, alice)
    device := env.seedDevice(t, aliceID, "prefs")
    projectKey := "project." + uuid.NewString()
    project := map[string]string{"label": "My project", "device_id": device, "command_id": "codex", "cwd": "C:/work", "name": ""}
    put := func(token, key string, value any) *apiResponse {
        return env.request(t, http.MethodPut, "/api/v1/preferences/"+key, token, map[string]any{"value": value})
    }
    if r := put(alice, projectKey, project); r.Status != 200 { t.Fatalf("save project: %d %s", r.Status, r.Body) }
    if r := put(alice, "font_size", 18); r.Status != 200 { t.Fatalf("save font: %d %s", r.Status, r.Body) }
    if r := env.get(t, "/api/v1/preferences", ""); r.Status != 401 { t.Fatalf("anonymous read: %d", r.Status) }
    if r := put(bob, projectKey, project); r.Status != 404 { t.Fatalf("foreign device accepted: %d", r.Status) }
    own := env.get(t, "/api/v1/preferences", alice).JSON(t)["values"].(map[string]any)
    if len(own) != 2 || own["font_size"] != float64(18) { t.Fatalf("independent keys lost: %#v", own) }
    if r := env.del(t, "/api/v1/preferences/"+projectKey, bob); r.Status != 204 { t.Fatalf("idempotent own delete: %d", r.Status) }
    if len(env.get(t, "/api/v1/preferences", bob).JSON(t)["values"].(map[string]any)) != 0 { t.Fatal("cross-account preferences leaked") }
    if len(env.get(t, "/api/v1/preferences", alice).JSON(t)["values"].(map[string]any)) != 2 { t.Fatal("another user deleted Alice preferences") }
    for _, value := range []any{0, 25, 14.5, "14", nil} {
        if r := put(alice, "font_size", value); r.Status != 400 { t.Fatalf("invalid font accepted: %#v status=%d", value, r.Status) }
    }
    sid := uuid.NewString()
    if err := env.store.UpsertSession(context.Background(), &storage.SessionMeta{ID: sid, UserID: aliceID, DeviceID: device, Status: "running", Cwd: "C:/work", Command: "codex", Cols: 80, Rows: 24, CreatedAt: env.clock.Now()}); err != nil { t.Fatal(err) }
    if r := put(alice, "pin."+sid, true); r.Status != 200 { t.Fatalf("pin own session: %d %s", r.Status, r.Body) }
    if r := put(bob, "pin."+sid, true); r.Status != 404 { t.Fatalf("pin foreign session: %d", r.Status) }
    if r := env.del(t, "/api/v1/preferences/pin."+sid, alice); r.Status != 204 { t.Fatalf("unpin: %d", r.Status) }
}
