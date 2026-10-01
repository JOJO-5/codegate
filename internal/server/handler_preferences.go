package server

import (
    "encoding/json"
    "net/http"
    "strings"

    "github.com/google/uuid"
)

type projectPreference struct {
    Label string `json:"label"`
    DeviceID string `json:"device_id"`
    CommandID string `json:"command_id"`
    Cwd string `json:"cwd"`
    Name string `json:"name"`
}

func (s *Server) handlePreferences(w http.ResponseWriter, r *http.Request) {
    userID, _ := userIDFromContext(r.Context())
    values, err := s.store.Preferences(r.Context(), userID)
    if err != nil { writeProtoError(w, err); return }
    out := make(map[string]json.RawMessage, len(values))
    for key, value := range values { out[key] = json.RawMessage(value) }
    writeJSON(w, http.StatusOK, map[string]any{"values": out})
}

func preferenceKey(key string) bool {
    if key == "font_size" { return true }
    if !strings.HasPrefix(key, "project.") && !strings.HasPrefix(key, "pin.") { return false }
    _, err := uuid.Parse(strings.SplitN(key, ".", 2)[1])
    return err == nil
}

func (s *Server) handlePreferenceSet(w http.ResponseWriter, r *http.Request) {
    userID, _ := userIDFromContext(r.Context())
    key := r.PathValue("key")
    if !preferenceKey(key) { writeError(w, 400, "invalid_preference", "不支持的偏好设置"); return }
    req, ok := decodeJSON[struct { Value json.RawMessage `json:"value"` }](w, r)
    if !ok { return }
    if len(req.Value) == 0 || len(req.Value) > 8192 { writeError(w, 400, "invalid_preference", "偏好内容过长或为空"); return }
    var value any
    switch {
    case key == "font_size":
        var size int
        if json.Unmarshal(req.Value, &size) != nil || size < 10 || size > 24 { writeError(w, 400, "invalid_preference", "字号范围为 10 至 24"); return }
        value = size
    case strings.HasPrefix(key, "pin."):
        var pinned bool
        if json.Unmarshal(req.Value, &pinned) != nil || !pinned { writeError(w, 400, "invalid_preference", "置顶值必须为 true"); return }
        session, err := s.store.SessionByID(r.Context(), strings.TrimPrefix(key, "pin."))
        if err != nil || session.UserID != userID { writeError(w, 404, "not_found", "会话不存在"); return }
        value = true
    case strings.HasPrefix(key, "project."):
        var project projectPreference
        if json.Unmarshal(req.Value, &project) != nil { writeError(w, 400, "invalid_preference", "项目格式错误"); return }
        project.Label = strings.TrimSpace(project.Label)
        project.Cwd = strings.TrimSpace(project.Cwd)
        if project.Label == "" || len(project.Label) > 160 || project.Cwd == "" || len(project.Cwd) > 4096 || project.CommandID == "" || len(project.CommandID) > 128 || len(project.Name) > 160 {
            writeError(w, 400, "invalid_preference", "请填写项目名称、工具和工作目录"); return
        }
        device, err := s.store.DeviceByID(r.Context(), project.DeviceID)
        if err != nil || device.UserID != userID { writeError(w, 404, "not_found", "设备不存在"); return }
        value = project
    }
    data, err := json.Marshal(value)
    if err != nil { writeProtoError(w, err); return }
    if err := s.store.SetPreference(r.Context(), userID, key, string(data)); err != nil { writeProtoError(w, err); return }
    writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": value})
}

func (s *Server) handlePreferenceDelete(w http.ResponseWriter, r *http.Request) {
    userID, _ := userIDFromContext(r.Context())
    key := r.PathValue("key")
    if !preferenceKey(key) { writeError(w, 400, "invalid_preference", "不支持的偏好设置"); return }
    if err := s.store.DeletePreference(r.Context(), userID, key); err != nil { writeProtoError(w, err); return }
    w.WriteHeader(http.StatusNoContent)
}
