package protocol

import "github.com/jojo/codegate/internal/transport"

type NetworkProbe struct {
	Enabled   bool   `json:"network_probe"`
	SessionID string `json:"session_id,omitempty"`
}

// Probe timing includes queueing and processing, not CLI echo or one-way delay.
type NetworkProbeResult struct {
	Version          int                 `json:"probe_version"`
	ServerAgentMS    *float64            `json:"server_agent_ms,omitempty"`
	ClientQueue      *transport.Snapshot `json:"client_queue,omitempty"`
	ServerAgentQueue *transport.Snapshot `json:"server_agent_queue,omitempty"`
	AgentQueue       *transport.Snapshot `json:"agent_queue,omitempty"`
}
