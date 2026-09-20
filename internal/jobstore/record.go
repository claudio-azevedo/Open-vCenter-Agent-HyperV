package jobstore

import (
	"encoding/json"
	"time"
)

// Job status constants representing the lifecycle states of a job.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
)

// JobRecord represents a single job persisted in the job store.
// It tracks the full lifecycle of a task from creation through completion.
type JobRecord struct {
	JobID        string     `json:"job_id"`
	TaskID       string     `json:"task_id"`
	TaskType     string     `json:"task_type"`
	Action       string     `json:"action,omitempty"`
	VMIdentifier string     `json:"vm_identifier,omitempty"`
	Payload      []byte     `json:"payload"`
	Status       string     `json:"status"`
	WorkerPID    int        `json:"worker_pid,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
}

// Marshal serializes a JobRecord to JSON bytes for storage.
func (j *JobRecord) Marshal() ([]byte, error) {
	return json.Marshal(j)
}

// UnmarshalJobRecord deserializes JSON bytes into a JobRecord.
func UnmarshalJobRecord(data []byte) (*JobRecord, error) {
	var record JobRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	return &record, nil
}
