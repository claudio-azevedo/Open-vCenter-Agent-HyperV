package jobstore

import (
	"fmt"
	"log/slog"
	"time"
)

// RecoverOrphanedJobs scans for jobs in "running" status with no live worker
// process and marks them as failed. This handles recovery after a service crash.
func (s *sqliteStore) RecoverOrphanedJobs(logger *slog.Logger) error {
	runningJobs, err := s.ListByStatus(StatusRunning)
	if err != nil {
		return fmt.Errorf("list running jobs: %w", err)
	}

	for _, job := range runningJobs {
		if job.WorkerPID == 0 {
			logger.Warn("recovering orphaned job with no PID",
				"job_id", job.JobID, "task_type", job.TaskType)
			if err := s.markJobFailed(job.JobID, "orphaned job: no worker PID recorded"); err != nil {
				logger.Error("failed to recover orphaned job", "job_id", job.JobID, "error", err)
			}
			continue
		}

		if !isProcessAlive(job.WorkerPID) {
			logger.Warn("recovering orphaned job with dead process",
				"job_id", job.JobID, "task_type", job.TaskType, "worker_pid", job.WorkerPID)
			if err := s.markJobFailed(job.JobID,
				fmt.Sprintf("orphaned job: worker process %d no longer running", job.WorkerPID)); err != nil {
				logger.Error("failed to recover orphaned job", "job_id", job.JobID, "error", err)
			}
		}
	}

	return nil
}

// markJobFailed transitions a job from running to failed with an error message.
func (s *sqliteStore) markJobFailed(jobID, errorMessage string) error {
	now := time.Now().UTC()
	return s.UpdateStatus(jobID, StatusRunning, StatusFailed, map[string]interface{}{
		"completed_at":  now,
		"error_message": errorMessage,
	})
}
