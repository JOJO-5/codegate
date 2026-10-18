package protocol

const (
	TypeWorkflowControl Type = "workflow.control"
	TypeWorkflowState   Type = "workflow.state"
)

// Profiles are resolved on the Agent from locally approved CLI commands.
type WorkflowProfile struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	CommandID string `json:"command_id"`
	Tool      string `json:"tool"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Free      bool   `json:"free"`
}
type WorkflowStep struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	ProfileID    string `json:"profile_id,omitempty"`
	ProfileLabel string `json:"profile_label,omitempty"`
	NativeID     string `json:"native_id,omitempty"`
	State        string `json:"state"`
	Instruction  string `json:"instruction,omitempty"`
	Output       string `json:"output,omitempty"`
}
type Workflow struct {
	ID             string         `json:"id"`
	UserID         string         `json:"-"`
	DeviceID       string         `json:"device_id"`
	Goal           string         `json:"goal"`
	Cwd            string         `json:"cwd"`
	MasterNativeID string         `json:"master_native_id,omitempty"`
	MasterID       string         `json:"master_id"`
	MasterLabel    string         `json:"master_label,omitempty"`
	FreeWorkers    bool           `json:"free_workers"`
	MaxTurns       int            `json:"max_turns"`
	MaxMinutes     int            `json:"max_minutes"`
	State          string         `json:"state"`
	Detail         string         `json:"detail,omitempty"`
	Worktree       string         `json:"worktree,omitempty"`
	Branch         string         `json:"branch,omitempty"`
	BaseCommit     string         `json:"base_commit,omitempty"`
	Revision       int64          `json:"revision"`
	AgentRevision  int64          `json:"agent_revision,omitempty"`
	Steps          []WorkflowStep `json:"steps"`
	Diff           string         `json:"diff,omitempty"`
	DiffTruncated  bool           `json:"diff_truncated,omitempty"`
	CreatedAt      int64          `json:"created_at"`
	UpdatedAt      int64          `json:"updated_at"`
}

func WorkflowTerminal(state string) bool {
	switch state {
	case "ready_for_review", "cancelled", "failed", "needs_review":
		return true
	}
	return false
}

type WorkflowControl struct {
	Action   string    `json:"action"`
	ID       string    `json:"id,omitempty"`
	UserID   string    `json:"user_id"`
	Workflow *Workflow `json:"workflow,omitempty"`
}
type WorkflowReply struct {
	Profiles []WorkflowProfile `json:"profiles,omitempty"`
	Workflow *Workflow         `json:"workflow,omitempty"`
}
