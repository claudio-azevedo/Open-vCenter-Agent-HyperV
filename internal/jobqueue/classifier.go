package jobqueue

import "ovc-agent/internal/jobstore"

// JobClass represents the classification of a job for concurrency control.
type JobClass int

const (
	ClassRead      JobClass = iota // No locking, runs freely
	ClassWrite                     // Per-VM mutual exclusion
	ClassHostWrite                 // Blocks all writes
	ClassDownload                  // Only max-concurrent limit
)

// Classify determines the concurrency class of a job based on its task type and action.
func Classify(job *jobstore.JobRecord) JobClass {
	switch job.TaskType {
	case "vm_inventory", "hardware_inventory", "vm_status",
		"template_inventory", "iso_inventory", "agent_status":
		return ClassRead

	case "host_management":
		switch job.Action {
		case "refresh_hardware", "refresh_inventory":
			return ClassRead
		default: // suspend, suspend_drain, resume, resume_fallback, restart
			return ClassHostWrite
		}

	case "artifact_download":
		return ClassDownload

	default: // vm_create, vm_clone, vm_edit, vm_management, vm_move, vm_rename, vm_export_template
		return ClassWrite
	}
}
