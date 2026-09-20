package jobqueue

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"ovc-agent/internal/jobstore"
)

// periodicRefreshPrefix marks a job the Scheduler created for one of its own
// periodic inventory/status/metrics refreshes. Such jobs have no backend Task,
// so the worker must not publish a response envelope for them.
const periodicRefreshPrefix = "refresh-"

// PeriodicRefreshTaskID builds the TaskID for a scheduled refresh of taskType.
func PeriodicRefreshTaskID(taskType string) string {
	return fmt.Sprintf("%s%s-%d", periodicRefreshPrefix, taskType, time.Now().Unix())
}

// IsPeriodicRefreshTaskID reports whether taskID was created by the Scheduler.
func IsPeriodicRefreshTaskID(taskID string) bool {
	return strings.HasPrefix(taskID, periodicRefreshPrefix)
}

// defaultHeartbeatInterval is used when SchedulerConfig.HeartbeatInterval is
// unset. It must stay below the backend's agent_offline_after_seconds.
const defaultHeartbeatInterval = 60 * time.Second

// SchedulerConfig holds the interval and delay settings for periodic refresh scheduling.
type SchedulerConfig struct {
	RefreshIntervalVMs  time.Duration // interval for vm_inventory (0 means disabled)
	RefreshIntervalHost time.Duration // interval for hardware_inventory, template_inventory, iso_inventory (0 means disabled)
	MetricsInterval     time.Duration // interval for host_metrics + vm_metrics (0 means disabled)
	HeartbeatInterval   time.Duration // interval for agent_status; always on (<=0 uses defaultHeartbeatInterval)
}

// Scheduler creates periodic refresh jobs in the job store following configured intervals
// and start delays. It replaces the previous direct-execution periodic refresh goroutines.
type Scheduler struct {
	store    jobstore.Store
	config   SchedulerConfig
	logger   *slog.Logger
	onNewJob func() // trigger dispatcher re-evaluation
}

// NewScheduler creates a new Scheduler instance.
func NewScheduler(store jobstore.Store, config SchedulerConfig, logger *slog.Logger, onNewJob func()) *Scheduler {
	return &Scheduler{
		store:    store,
		config:   config,
		logger:   logger,
		onNewJob: onNewJob,
	}
}

// refreshSpec defines a periodic refresh type with its interval and start delay.
type refreshSpec struct {
	taskType string
	interval time.Duration
	delay    time.Duration
}

// buildSpecs returns the periodic refresh specs derived from the scheduler
// config. agent_status runs on its own short heartbeat cadence rather than
// refresh_interval_host (which can be 10+ minutes, far longer than the
// backend's offline cutoff).
func (s *Scheduler) buildSpecs() []refreshSpec {
	heartbeat := s.config.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeatInterval
	}

	return []refreshSpec{
		{taskType: "vm_inventory", interval: s.config.RefreshIntervalVMs, delay: 0},
		{taskType: "hardware_inventory", interval: s.config.RefreshIntervalHost, delay: 30 * time.Second},
		{taskType: "template_inventory", interval: s.config.RefreshIntervalHost, delay: 60 * time.Second},
		{taskType: "iso_inventory", interval: s.config.RefreshIntervalHost, delay: 90 * time.Second},
		{taskType: "agent_status", interval: heartbeat, delay: 15 * time.Second},
		{taskType: "vm_metrics", interval: s.config.MetricsInterval, delay: 20 * time.Second},
		{taskType: "host_metrics", interval: s.config.MetricsInterval, delay: 45 * time.Second},
	}
}

// Run starts the periodic refresh scheduling. It launches a goroutine for each enabled
// refresh type and blocks until the context is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for _, spec := range s.buildSpecs() {
		if spec.interval == 0 {
			s.logger.Info("periodic refresh disabled", "task_type", spec.taskType)
			continue
		}
		go s.runRefreshLoop(ctx, spec)
	}

	// Block until context is cancelled.
	<-ctx.Done()
}

// runRefreshLoop waits for the initial delay, then ticks at the configured interval,
// creating a refresh job on each tick if one doesn't already exist.
func (s *Scheduler) runRefreshLoop(ctx context.Context, spec refreshSpec) {
	// Wait for start delay.
	if spec.delay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(spec.delay):
		}
	}

	// Create the first refresh job immediately after delay.
	s.maybeCreateRefreshJob(ctx, spec.taskType)

	ticker := time.NewTicker(spec.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maybeCreateRefreshJob(ctx, spec.taskType)
		}
	}
}

// maybeCreateRefreshJob checks if a job of the given type already exists in queued or
// running status. If not, it creates a new job record and signals the dispatcher.
func (s *Scheduler) maybeCreateRefreshJob(ctx context.Context, taskType string) {
	// Check context before proceeding.
	if ctx.Err() != nil {
		return
	}

	if s.hasExistingRefresh(taskType) {
		s.logger.Debug("skipping periodic refresh, existing job found",
			"task_type", taskType,
		)
		return
	}

	job := &jobstore.JobRecord{
		JobID:     generateJobID(),
		TaskID:    PeriodicRefreshTaskID(taskType),
		TaskType:  taskType,
		Status:    jobstore.StatusQueued,
		CreatedAt: time.Now().UTC(),
	}

	if err := s.store.Create(job); err != nil {
		s.logger.Error("failed to create periodic refresh job",
			"task_type", taskType,
			"error", err,
		)
		return
	}

	s.logger.Info("created periodic refresh job",
		"task_type", taskType,
		"job_id", job.JobID,
	)

	if s.onNewJob != nil {
		s.onNewJob()
	}
}

// hasExistingRefresh checks whether a job of the given task type already exists
// in queued or running status.
func (s *Scheduler) hasExistingRefresh(taskType string) bool {
	for _, status := range []string{jobstore.StatusQueued, jobstore.StatusRunning} {
		jobs, err := s.store.ListByStatus(status)
		if err != nil {
			s.logger.Error("failed to list jobs by status",
				"status", status,
				"error", err,
			)
			continue
		}
		for _, job := range jobs {
			if job.TaskType == taskType {
				return true
			}
		}
	}
	return false
}

// generateJobID creates a random UUID v4 string for use as a job ID.
func generateJobID() string {
	var uuid [16]byte
	rand.Read(uuid[:])
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}
