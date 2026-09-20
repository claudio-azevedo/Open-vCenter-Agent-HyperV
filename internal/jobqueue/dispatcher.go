package jobqueue

import (
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"ovc-agent/internal/jobstore"
)

// SpawnFunc is a function that handles spawning a worker for a job.
// If nil on the Dispatcher, the default os.Executable + exec.Command logic is used.
// Used in tests to intercept dispatch calls without spawning real processes.
type SpawnFunc func(job *jobstore.JobRecord) error

// Dispatcher evaluates concurrency rules and spawns worker processes for eligible jobs.
// It runs on a single goroutine to avoid parallel dispatch decisions that could violate
// mutual exclusion rules.
type Dispatcher struct {
	mu                sync.Mutex
	store             jobstore.Store
	monitor           *Monitor
	logger            *slog.Logger
	maxConcurrentJobs int
	drainMode         bool
	triggerCh         chan struct{}
	spawnFunc         SpawnFunc // if non-nil, called instead of real process spawn
}

// NewDispatcher creates a new Dispatcher instance.
func NewDispatcher(store jobstore.Store, monitor *Monitor, maxConcurrentJobs int, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		store:             store,
		monitor:           monitor,
		logger:            logger,
		maxConcurrentJobs: maxConcurrentJobs,
		triggerCh:         make(chan struct{}, 1),
	}
}

// TriggerEvaluation sends a non-blocking signal to the dispatch loop to re-evaluate
// queued jobs. Safe to call from any goroutine.
func (d *Dispatcher) TriggerEvaluation() {
	select {
	case d.triggerCh <- struct{}{}:
	default:
		// Channel already has a pending signal, no need to send another.
	}
}

// RunLoop runs the dispatch evaluation loop until the context is cancelled.
// It selects on the trigger channel, a 2-second safety-net ticker, and context cancellation.
func (d *Dispatcher) RunLoop(ctx <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx:
			return
		case <-d.triggerCh:
			d.EvaluateAndDispatch()
		case <-ticker.C:
			d.EvaluateAndDispatch()
		}
	}
}

// EvaluateAndDispatch is the core dispatch logic. It checks concurrency rules and
// dispatches eligible queued jobs. It must be called from a single goroutine (the RunLoop).
func (d *Dispatcher) EvaluateAndDispatch() {
	d.mu.Lock()
	if d.drainMode {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()

	runningCount := d.monitor.RunningCount()
	if runningCount >= d.maxConcurrentJobs {
		return
	}

	// If a host write is running, only dispatch read jobs.
	if d.monitor.HasRunningHostWrite() {
		d.dispatchEligibleReads(d.maxConcurrentJobs - runningCount)
		return
	}

	hasRunningWrite := d.monitor.HasRunningWrite()

	queued, err := d.store.ListByStatus(jobstore.StatusQueued)
	if err != nil {
		d.logger.Error("failed to list queued jobs", "error", err)
		return
	}

	for _, job := range queued {
		if runningCount >= d.maxConcurrentJobs {
			break
		}

		class := Classify(job)

		switch class {
		case ClassRead:
			d.dispatch(job)
			runningCount++

		case ClassWrite:
			if !d.monitor.HasRunningWriteForVM(job.VMIdentifier) {
				d.dispatch(job)
				runningCount++
			}

		case ClassHostWrite:
			if !hasRunningWrite && !d.monitor.HasRunningHostWrite() {
				d.dispatch(job)
				runningCount++
				hasRunningWrite = true
			}

		case ClassDownload:
			d.dispatch(job)
			runningCount++
		}
	}
}

// dispatchEligibleReads dispatches only Read_Jobs from the queue, up to the given slot limit.
// Called when a host write is running and only reads are allowed.
func (d *Dispatcher) dispatchEligibleReads(slots int) {
	if slots <= 0 {
		return
	}

	queued, err := d.store.ListByStatus(jobstore.StatusQueued)
	if err != nil {
		d.logger.Error("failed to list queued jobs for read dispatch", "error", err)
		return
	}

	dispatched := 0
	for _, job := range queued {
		if dispatched >= slots {
			break
		}

		if Classify(job) == ClassRead {
			d.dispatch(job)
			dispatched++
		}
	}
}

// dispatch spawns a worker process for the given job. The correct sequence is:
//  1. Spawn the process (or call spawnFunc if set)
//  2. If spawn succeeds: update status to "running" with PID, register with monitor
//  3. If spawn fails: leave job as "queued", log the error
func (d *Dispatcher) dispatch(job *jobstore.JobRecord) {
	// If a custom spawn function is set (e.g., for testing), use it instead of real spawn.
	if d.spawnFunc != nil {
		if err := d.spawnFunc(job); err != nil {
			d.logger.Error("spawnFunc failed",
				"job_id", job.JobID,
				"error", err,
			)
		}
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		d.logger.Error("failed to determine executable path for worker spawn",
			"job_id", job.JobID,
			"error", err,
		)
		return
	}

	cmd := exec.Command(exePath, "job", job.JobID)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		d.logger.Error("failed to spawn worker process",
			"job_id", job.JobID,
			"task_type", job.TaskType,
			"error", err,
		)
		// Leave job as "queued" - it will be retried on the next dispatch cycle.
		return
	}

	pid := cmd.Process.Pid
	now := time.Now().UTC()

	// Spawn succeeded - update job status to "running".
	err = d.store.UpdateStatus(job.JobID, jobstore.StatusQueued, jobstore.StatusRunning, map[string]interface{}{
		"started_at": now,
		"worker_pid": pid,
	})
	if err != nil {
		d.logger.Error("failed to update job status to running after spawn",
			"job_id", job.JobID,
			"pid", pid,
			"error", err,
		)
		// Kill the spawned process since we can't track it properly.
		cmd.Process.Kill()
		return
	}

	// Register the worker entry with the monitor for tracking.
	entry := &WorkerEntry{
		JobID:        job.JobID,
		TaskID:       job.TaskID,
		PID:          pid,
		Process:      cmd.Process,
		Cmd:          cmd,
		StartedAt:    now,
		TaskType:     job.TaskType,
		Action:       job.Action,
		VMIdentifier: job.VMIdentifier,
		Class:        Classify(job),
	}
	d.monitor.Register(entry)

	d.logger.Info("dispatched worker",
		"job_id", job.JobID,
		"task_type", job.TaskType,
		"action", job.Action,
		"vm_identifier", job.VMIdentifier,
		"pid", pid,
	)
}

// EnterDrainMode sets the dispatcher to drain mode for an agent upgrade.
// No new workers will be dispatched. A background goroutine waits for all running
// workers to finish (with a 30-minute hard timeout) before launching the upgrade.
func (d *Dispatcher) EnterDrainMode(binaryPath string) {
	d.mu.Lock()
	d.drainMode = true
	d.mu.Unlock()

	d.logger.Info("entered drain mode, waiting for workers to finish",
		"pending_upgrade", binaryPath,
	)

	go func() {
		timeout := time.After(30 * time.Minute)
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				d.logger.Warn("drain mode timeout reached (30 minutes), force-proceeding with upgrade",
					"running_workers", d.monitor.RunningCount(),
				)
				d.launchUpgrade(binaryPath)
				return
			case <-ticker.C:
				if d.monitor.RunningCount() == 0 {
					d.logger.Info("all workers finished, proceeding with upgrade")
					d.launchUpgrade(binaryPath)
					return
				}
			}
		}
	}()
}

// ExitDrainMode restores normal dispatching. Called if the upgrade fails.
func (d *Dispatcher) ExitDrainMode() {
	d.mu.Lock()
	d.drainMode = false
	d.mu.Unlock()

	d.logger.Info("exited drain mode, resuming normal dispatch")
	d.TriggerEvaluation()
}

// InDrainMode returns whether the dispatcher is currently in drain mode.
func (d *Dispatcher) InDrainMode() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.drainMode
}

// launchUpgrade starts the upgrade process using the downloaded binary.
func (d *Dispatcher) launchUpgrade(binaryPath string) {
	cmd := exec.Command(binaryPath, "upgrade")
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		d.logger.Error("failed to launch upgrade process",
			"binary_path", binaryPath,
			"error", err,
		)
		// Exit drain mode on failure so dispatching resumes.
		d.ExitDrainMode()
		return
	}

	d.logger.Info("upgrade process launched",
		"binary_path", binaryPath,
		"pid", cmd.Process.Pid,
	)
}
