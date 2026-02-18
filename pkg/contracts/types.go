package contracts

import "time"

// RunStatus mirrors LangGraph-compatible run lifecycle values.
type RunStatus string

const (
	RunStatusPending     RunStatus = "pending"
	RunStatusRunning     RunStatus = "running"
	RunStatusError       RunStatus = "error"
	RunStatusSuccess     RunStatus = "success"
	RunStatusTimeout     RunStatus = "timeout"
	RunStatusInterrupted RunStatus = "interrupted"
)

// MultitaskStrategy defines behavior for competing runs on the same thread.
type MultitaskStrategy string

const (
	MultitaskReject    MultitaskStrategy = "reject"
	MultitaskRollback  MultitaskStrategy = "rollback"
	MultitaskInterrupt MultitaskStrategy = "interrupt"
	MultitaskEnqueue   MultitaskStrategy = "enqueue"
)

type Assistant struct {
	ID           string            `json:"id"`
	DeploymentID string            `json:"deployment_id"`
	GraphID      string            `json:"graph_id"`
	Config       map[string]string `json:"config,omitempty"`
	Version      string            `json:"version"`
}

type Thread struct {
	ID          string            `json:"id"`
	AssistantID string            `json:"assistant_id"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type Run struct {
	ID                string            `json:"id"`
	ThreadID          string            `json:"thread_id,omitempty"`
	AssistantID       string            `json:"assistant_id"`
	Status            RunStatus         `json:"status"`
	MultitaskStrategy MultitaskStrategy `json:"multitask_strategy"`
	StreamResumable   bool              `json:"stream_resumable"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type CronJob struct {
	ID          string `json:"id"`
	AssistantID string `json:"assistant_id"`
	Schedule    string `json:"schedule"`
	Enabled     bool   `json:"enabled"`
}

type ProjectRole string

const (
	RoleViewer    ProjectRole = "viewer"
	RoleDeveloper ProjectRole = "developer"
	RoleOperator  ProjectRole = "operator"
	RoleAdmin     ProjectRole = "admin"
)

func IsValidProjectRole(role ProjectRole) bool {
	switch role {
	case RoleViewer, RoleDeveloper, RoleOperator, RoleAdmin:
		return true
	default:
		return false
	}
}

const (
	HeaderAPIKey      = "X-Api-Key"
	HeaderAuthScheme  = "X-Auth-Scheme"
	HeaderRequestID   = "X-Request-Id"
	HeaderProjectRole = "X-Project-Role"
)
