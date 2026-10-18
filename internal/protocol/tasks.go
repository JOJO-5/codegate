package protocol

// ContinuationTask binds each run to an owned native conversation. Prompt goes
// to the CLI stdin, not to an executable command line.
type ContinuationTask struct {
	DeletedAt        int64          `json:"deleted_at,omitempty"`
	Progress         *TaskProgress  `json:"progress,omitempty"`
	QuotaWaitMinutes int            `json:"quota_wait_minutes,omitempty"`
	MaxRetries       int            `json:"max_retries,omitempty"`
	RetryErrors      bool           `json:"retry_errors,omitempty"`
	Retry            bool           `json:"retry,omitempty"`
	ID               string         `json:"id"`
	UserID           string         `json:"-"`
	DeviceID         string         `json:"device_id"`
	SessionID        string         `json:"session_id"`
	Source           SessionSummary `json:"source"`
	Prompt           string         `json:"prompt"`
	Mode             string         `json:"mode"`
	DueAt            int64          `json:"due_at"`
	State            string         `json:"state"`
	Attempt          int            `json:"attempt"`
	MaxAttempts      int            `json:"max_attempts"`
	MaxRunMinutes    int            `json:"max_run_minutes"`
	CreatedAt        int64          `json:"created_at"`
	UpdatedAt        int64          `json:"updated_at"`
	Detail           string         `json:"detail,omitempty"`
	Output           string         `json:"output,omitempty"`
}
type TaskRunPayload struct {
	QuotaWaitMinutes int            `json:"quota_wait_minutes,omitempty"`
	MaxRetries       int            `json:"max_retries,omitempty"`
	RetryErrors      bool           `json:"retry_errors,omitempty"`
	TaskID           string         `json:"task_id"`
	UserID           string         `json:"user_id"`
	Attempt          int            `json:"attempt"`
	Source           SessionSummary `json:"source"`
	Prompt           string         `json:"prompt,omitempty"`
	Mode             string         `json:"mode,omitempty"`
	MaxRunMinutes    int            `json:"max_run_minutes,omitempty"`
}
type TaskStatePayload struct {
	Progress *TaskProgress `json:"progress,omitempty"`
	Retry    bool          `json:"retry,omitempty"`
	TaskID   string        `json:"task_id"`
	Attempt  int           `json:"attempt"`
	State    string        `json:"state"`
	DueAt    int64         `json:"due_at,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	Output   string        `json:"output,omitempty"`
}

// TaskProgress contains safe, bounded activity, never raw tool arguments/stderr.
type TaskProgress struct {
	Sequence   int64  `json:"sequence"`
	StartedAt  int64  `json:"started_at"`
	ObservedAt int64  `json:"observed_at"`
	ActivityAt int64  `json:"activity_at,omitempty"`
	Activity   string `json:"activity"`
	Text       string `json:"text,omitempty"`
}
