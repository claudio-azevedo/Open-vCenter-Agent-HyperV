package jobstore

import (
	"log/slog"
	"time"
)

// Store defines the interface for job persistence operations.
// All write operations are transactional. UpdateStatus uses compare-and-swap
// semantics to prevent invalid state transitions.
type Store interface {
	Create(job *JobRecord) error
	GetByID(jobID string) (*JobRecord, error)
	UpdateStatus(jobID string, from, to string, fields map[string]interface{}) error
	ListByStatus(status string) ([]*JobRecord, error)
	ListRunningByVM(vmIdentifier string) ([]*JobRecord, error)
	HasConflict(taskType, action, vmIdentifier string) (bool, error)
	DeleteOlderThan(age time.Duration, statuses []string) (int, error)
	RecoverOrphanedJobs(logger *slog.Logger) error
	Close() error
}

// applyFields sets optional fields on a job record from a map.
// Supported keys: "started_at", "completed_at", "worker_pid", "error_message".
func applyFields(record *JobRecord, fields map[string]interface{}) {
	if v, ok := fields["started_at"]; ok {
		if t, ok := v.(time.Time); ok {
			record.StartedAt = &t
		}
	}
	if v, ok := fields["completed_at"]; ok {
		if t, ok := v.(time.Time); ok {
			record.CompletedAt = &t
		}
	}
	if v, ok := fields["worker_pid"]; ok {
		if pid, ok := v.(int); ok {
			record.WorkerPID = pid
		}
	}
	if v, ok := fields["error_message"]; ok {
		if msg, ok := v.(string); ok {
			record.ErrorMessage = msg
		}
	}
}
