package protocol

import (
	"encoding/json"
	"strings"
)

// BulkTraffic keeps file and web lifecycle controls with their data. Never
// classify stdout/replay independently of session controls: ordering matters.
func BulkTraffic(binary bool, data []byte) bool {
	if binary {
		if len(data) < BinaryHeaderLen {
			return false
		}
		switch FrameType(data[1]) {
		case FrameFileData, FrameWebToAgent, FrameWebToServer:
			return true
		}
		return false
	}
	var env struct {
		Type Type `json:"type"`
	}
	if json.Unmarshal(data, &env) != nil {
		return false
	}
	return strings.HasPrefix(string(env.Type), "file.") || strings.HasPrefix(string(env.Type), "web.")
}
