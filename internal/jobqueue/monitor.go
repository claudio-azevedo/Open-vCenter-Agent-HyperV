package jobqueue

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"ovc-agent/internal/jobstore"
)

// WorkerEntry represents a running worker process tracked by the Monitor.
type WorkerEntry struct {
	JobID        string
	TaskID       string
	PID          int
	Process      *os.Process
	Cmd          *exec.Cmd
	StartedAt    time.Time
	TaskType     string
	Action       string
	VMIdentifier string
	Class        JobClass
}

// Monitor tracks running worker processes, detects termination, and enforces timeouts.
type Monitor struct {
	mu             sync.Mutex
	workers        map[string]*WorkerEntry // key = job_id
	store          jobstore.Store
	logger         *slog.Logger
	onFinish       func()                                  // trigger dispatcher re-evaluation
	onJobCompleted func(entry *WorkerEntry)                // called when a worker exits and job status is "completed"
	onWorkerFailed func(entry *WorkerEntry, reason string) // called when a worker is killed or terminates unexpectedly
	defaultTimeout time.Duration
}

// NewMonitor creates a new Monitor instance.
func NewMonitor(store jobstore.Store, logger *slog.Logger, defaultTimeout time.Duration, onFinish func()) *Monitor {
	return &Monitor{
		workers:        make(map[string]*WorkerEntry),
		store:          store,
		logger:         logger,
		onFinish:       onFinish,
		defaultTimeout: defaultTimeout,
	}
}

// SetOnJobCompleted registers a callback that fires when a worker exits and the job
// status in the store is "completed". This enables the service to react to successful
// job completions (e.g., triggering drain mode for agent upgrades).
func (m *Monitor) SetOnJobCompleted(fn func(entry *WorkerEntry)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onJobCompleted = fn
}

// SetOnWorkerFailed registers a callback that fires when a worker is killed by timeout
// or terminates unexpectedly while the job was still "running". This enables the service
// to publish failure responses to the backend via RabbitMQ.
func (m *Monitor) SetOnWorkerFailed(fn func(entry *WorkerEntry, reason string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onWorkerFailed = fn
}

// Register adds a worker entry to the registry and starts a goroutine to wait
// for the worker process to terminate.
func (m *Monitor) Register(entry *WorkerEntry) {
	m.mu.Lock()
	m.workers[entry.JobID] = entry
	m.mu.Unlock()

	go func(e *WorkerEntry) {
		_ = e.Cmd.Wait() // blocks until process exits
		m.handleTermination(e)
	}(entry)
}

// handleTermination is called when a worker process exits. It checks the job
// status in the database and marks it as failed if it's still "running",
// removes the entry from the registry, and triggers dispatcher re-evaluation.
// If the job completed successfully, the onJobCompleted callback is invoked.
func (m *Monitor) handleTermination(entry *WorkerEntry) {
	m.mu.Lock()

	// Check job status in DB
	job, err := m.store.GetByID(entry.JobID)
	if err != nil {
		m.logger.Error("failed to read job status during worker termination",
			"job_id", entry.JobID,
			"error", err,
		)
		// Remove from registry even if DB read fails
		delete(m.workers, entry.JobID)
		m.mu.Unlock()

		if m.onFinish != nil {
			m.onFinish()
		}
		return
	}

	// If still "running", the worker terminated unexpectedly - mark as failed
	var failedWhileRunning bool
	if job.Status == jobstore.StatusRunning {
		failedWhileRunning = true
		now := time.Now().UTC()
		updateErr := m.store.UpdateStatus(entry.JobID, jobstore.StatusRunning, jobstore.StatusFailed, map[string]interface{}{
			"completed_at":  now,
			"error_message": "unexpected worker termination",
		})
		if updateErr != nil {
			m.logger.Error("failed to mark job as failed after unexpected termination",
				"job_id", entry.JobID,
				"error", updateErr,
			)
		} else {
			m.logger.Warn("worker terminated unexpectedly, marked job as failed",
				"job_id", entry.JobID,
				"pid", entry.PID,
				"task_type", entry.TaskType,
			)
		}
	}

	// Capture callbacks while holding the lock
	onJobCompleted := m.onJobCompleted
	onWorkerFailed := m.onWorkerFailed
	delete(m.workers, entry.JobID)
	m.mu.Unlock()

	if m.onFinish != nil {
		m.onFinish()
	}

	// If the worker failed while the job was still running, notify the failure callback
	// so the service can publish the failure response to the backend.
	if failedWhileRunning && onWorkerFailed != nil {
		onWorkerFailed(entry, "unexpected worker termination")
	}

	// If the job completed successfully, notify the onJobCompleted callback
	if job.Status == jobstore.StatusCompleted && onJobCompleted != nil {
		onJobCompleted(entry)
	}
}

// RunningCount returns the number of currently running worker processes.
func (m *Monitor) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.workers)
}

// HasRunningHostWrite returns true if any running worker has ClassHostWrite.
func (m *Monitor) HasRunningHostWrite() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.workers {
		if entry.Class == ClassHostWrite {
			return true
		}
	}
	return false
}

// HasRunningWrite returns true if any running worker has ClassWrite or ClassHostWrite.
func (m *Monitor) HasRunningWrite() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.workers {
		if entry.Class == ClassWrite || entry.Class == ClassHostWrite {
			return true
		}
	}
	return false
}

// HasRunningWriteForVM returns true if any running worker has ClassWrite and
// matches the given VM identifier.
func (m *Monitor) HasRunningWriteForVM(vmID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.workers {
		if entry.Class == ClassWrite && entry.VMIdentifier == vmID {
			return true
		}
	}
	return false
}

// Run starts the timeout enforcement ticker. It runs until the context is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkTimeouts()
		}
	}
}

// checkTimeouts iterates over running workers and kills any that have exceeded
// their task-type specific timeout.
func (m *Monitor) checkTimeouts() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for _, entry := range m.workers {
		timeout := m.getTimeoutForTask(entry.TaskType, entry.Action)
		if now.Sub(entry.StartedAt) > timeout {
			m.logger.Warn("killing worker due to timeout",
				"job_id", entry.JobID,
				"pid", entry.PID,
				"task_type", entry.TaskType,
				"action", entry.Action,
				"elapsed", now.Sub(entry.StartedAt).String(),
				"timeout", timeout.String(),
			)
			if err := entry.Process.Kill(); err != nil {
				m.logger.Error("failed to kill timed-out worker",
					"job_id", entry.JobID,
					"pid", entry.PID,
					"error", err,
				)
			}
			// The Wait() goroutine will detect the exit and call handleTermination
		}
	}
}

// Per-task-type timeout overrides.
var taskTimeouts = map[string]time.Duration{
	"vm_clone": 1800 * time.Second,
	"vm_edit":  1800 * time.Second,
	// Creating Fixed VHDs zero-fills the file - ~10s/GB, so a large OS disk can
	// run well past the default. vm_create.go also caps itself internally.
	"vm_create":          1800 * time.Second,
	"vm_inventory":       600 * time.Second,
	"hardware_inventory": 600 * time.Second,
	"template_inventory": 600 * time.Second,
	"iso_inventory":      300 * time.Second,
	"artifact_download":  3600 * time.Second,
}

// Action-based timeout overrides for vm_management and host_management sub-actions.
var actionTimeouts = map[string]time.Duration{
	"vm_batch_start":     1800 * time.Second,
	"vm_batch_stop":      1800 * time.Second,
	"vm_move":            1800 * time.Second,
	"vm_rename":          1800 * time.Second,
	"vm_export_template": 1800 * time.Second,
	"suspend_drain":      600 * time.Second,
	// A graceful guest-OS shutdown can take several minutes (pending Windows
	// updates, slow-stopping services). The PS script self-limits below this.
	"vm_shutdown": 600 * time.Second,
}

// getTimeoutForTask returns the timeout for a given task type and action.
// It first checks the action-level overrides (for vm_management sub-actions),
// then the task-type overrides, and falls back to the configured default.
func (m *Monitor) getTimeoutForTask(taskType, action string) time.Duration {
	// Check action-specific timeouts (e.g., vm_move, vm_batch_start, suspend_drain)
	if action != "" {
		if t, ok := actionTimeouts[action]; ok {
			return t
		}
	}

	// Check task-type-level timeouts (e.g., vm_clone, vm_inventory, iso_inventory)
	if t, ok := taskTimeouts[taskType]; ok {
		return t
	}

	// Fall back to configured default
	return m.defaultTimeout
}
