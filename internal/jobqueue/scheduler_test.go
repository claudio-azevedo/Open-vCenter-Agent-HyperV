package jobqueue

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ovc-agent/internal/jobstore"

	"github.com/stretchr/testify/assert"
)

// schedulerMockStore implements jobstore.Store for scheduler testing,
// with a functional ListByStatus that filters jobs.
type schedulerMockStore struct {
	mu   sync.Mutex
	jobs map[string]*jobstore.JobRecord
}

func newSchedulerMockStore() *schedulerMockStore {
	return &schedulerMockStore{
		jobs: make(map[string]*jobstore.JobRecord),
	}
}

func (s *schedulerMockStore) Create(job *jobstore.JobRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.JobID] = job
	return nil
}

func (s *schedulerMockStore) GetByID(jobID string) (*jobstore.JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, os.ErrNotExist
	}
	return job, nil
}

func (s *schedulerMockStore) UpdateStatus(jobID string, from, to string, fields map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return os.ErrNotExist
	}
	if job.Status != from {
		return os.ErrInvalid
	}
	job.Status = to
	return nil
}

func (s *schedulerMockStore) ListByStatus(status string) ([]*jobstore.JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []*jobstore.JobRecord
	for _, job := range s.jobs {
		if job.Status == status {
			result = append(result, job)
		}
	}
	return result, nil
}

func (s *schedulerMockStore) ListRunningByVM(_ string) ([]*jobstore.JobRecord, error) {
	return nil, nil
}

func (s *schedulerMockStore) HasConflict(_, _, _ string) (bool, error) {
	return false, nil
}

func (s *schedulerMockStore) DeleteOlderThan(_ time.Duration, _ []string) (int, error) {
	return 0, nil
}

func (s *schedulerMockStore) RecoverOrphanedJobs(_ *slog.Logger) error {
	return nil
}

func (s *schedulerMockStore) Close() error {
	return nil
}

func (s *schedulerMockStore) jobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

func (s *schedulerMockStore) jobsByType(taskType string) []*jobstore.JobRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []*jobstore.JobRecord
	for _, job := range s.jobs {
		if job.TaskType == taskType {
			result = append(result, job)
		}
	}
	return result
}

func schedulerTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestScheduler_NewScheduler(t *testing.T) {
	store := newSchedulerMockStore()
	cfg := SchedulerConfig{
		RefreshIntervalVMs:  5 * time.Minute,
		RefreshIntervalHost: 10 * time.Minute,
	}
	onNewJob := func() {}

	s := NewScheduler(store, cfg, schedulerTestLogger(), onNewJob)

	assert.NotNil(t, s)
	assert.Equal(t, cfg.RefreshIntervalVMs, s.config.RefreshIntervalVMs)
	assert.Equal(t, cfg.RefreshIntervalHost, s.config.RefreshIntervalHost)
}

func TestScheduler_DisabledInterval_NoJobsCreated(t *testing.T) {
	store := newSchedulerMockStore()
	cfg := SchedulerConfig{
		RefreshIntervalVMs:  0, // disabled
		RefreshIntervalHost: 0, // disabled
	}

	s := NewScheduler(store, cfg, schedulerTestLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	// No jobs should be created when intervals are 0
	assert.Equal(t, 0, store.jobCount())
}

func TestScheduler_CreatesJobsForEnabledRefreshTypes(t *testing.T) {
	store := newSchedulerMockStore()
	var signalCount atomic.Int32
	onNewJob := func() { signalCount.Add(1) }

	cfg := SchedulerConfig{
		RefreshIntervalVMs:  50 * time.Millisecond,
		RefreshIntervalHost: 50 * time.Millisecond,
	}

	s := NewScheduler(store, cfg, schedulerTestLogger(), onNewJob)

	// Run with a short context - long enough for vm_inventory immediate tick
	// but short enough that delayed tasks (30s, 60s, 90s) won't fire.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	// vm_inventory should have created at least 1 job (immediate, no delay)
	vmJobs := store.jobsByType("vm_inventory")
	assert.GreaterOrEqual(t, len(vmJobs), 1, "expected at least 1 vm_inventory job")

	// Verify job properties
	if len(vmJobs) > 0 {
		job := vmJobs[0]
		assert.Equal(t, "vm_inventory", job.TaskType)
		assert.Equal(t, jobstore.StatusQueued, job.Status)
		assert.Equal(t, "", job.VMIdentifier)
		assert.False(t, job.CreatedAt.IsZero())
		assert.NotEmpty(t, job.JobID)
	}

	// onNewJob should have been signaled
	assert.GreaterOrEqual(t, signalCount.Load(), int32(1))
}

func TestScheduler_SkipsCreationIfJobAlreadyQueued(t *testing.T) {
	store := newSchedulerMockStore()
	cfg := SchedulerConfig{
		RefreshIntervalVMs:  50 * time.Millisecond,
		RefreshIntervalHost: 0, // disabled
	}

	// Pre-create a vm_inventory job in queued status
	store.Create(&jobstore.JobRecord{
		JobID:    "existing-job-1",
		TaskType: "vm_inventory",
		Status:   jobstore.StatusQueued,
	})

	s := NewScheduler(store, cfg, schedulerTestLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	// Only the pre-existing job should remain; no new vm_inventory job created.
	vmJobs := store.jobsByType("vm_inventory")
	assert.Equal(t, 1, len(vmJobs))
	assert.Equal(t, "existing-job-1", vmJobs[0].JobID)
}

func TestScheduler_SkipsCreationIfJobAlreadyRunning(t *testing.T) {
	store := newSchedulerMockStore()
	cfg := SchedulerConfig{
		RefreshIntervalVMs:  50 * time.Millisecond,
		RefreshIntervalHost: 0, // disabled
	}

	// Pre-create a vm_inventory job in running status
	store.Create(&jobstore.JobRecord{
		JobID:    "running-job-1",
		TaskType: "vm_inventory",
		Status:   jobstore.StatusRunning,
	})

	s := NewScheduler(store, cfg, schedulerTestLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	// Only the pre-existing job should remain.
	vmJobs := store.jobsByType("vm_inventory")
	assert.Equal(t, 1, len(vmJobs))
	assert.Equal(t, "running-job-1", vmJobs[0].JobID)
}

func TestScheduler_CreatesJobAfterExistingCompletes(t *testing.T) {
	store := newSchedulerMockStore()
	cfg := SchedulerConfig{
		RefreshIntervalVMs:  50 * time.Millisecond,
		RefreshIntervalHost: 0, // disabled
	}

	// Pre-create a completed vm_inventory job - should NOT block new creation
	store.Create(&jobstore.JobRecord{
		JobID:    "completed-job-1",
		TaskType: "vm_inventory",
		Status:   jobstore.StatusCompleted,
	})

	s := NewScheduler(store, cfg, schedulerTestLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	// A new vm_inventory job should have been created
	vmJobs := store.jobsByType("vm_inventory")
	assert.GreaterOrEqual(t, len(vmJobs), 2, "expected new job alongside completed one")
}

func TestScheduler_Run_RespectsContextCancellation(t *testing.T) {
	store := newSchedulerMockStore()
	cfg := SchedulerConfig{
		RefreshIntervalVMs:  1 * time.Hour,
		RefreshIntervalHost: 1 * time.Hour,
	}

	s := NewScheduler(store, cfg, schedulerTestLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		s.Run(ctx)
		close(done)
	}()

	// Cancel immediately
	cancel()

	select {
	case <-done:
		// success - Run exited promptly
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler.Run did not exit after context cancellation")
	}
}

func TestScheduler_GenerateJobID_UniqueAndFormatted(t *testing.T) {
	ids := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := generateJobID()
		assert.Len(t, id, 36, "UUID v4 should be 36 chars long")
		assert.Contains(t, id, "-")
		_, exists := ids[id]
		assert.False(t, exists, "generated duplicate ID")
		ids[id] = struct{}{}
	}
}

func TestScheduler_HasExistingRefresh_IgnoresOtherTaskTypes(t *testing.T) {
	store := newSchedulerMockStore()
	cfg := SchedulerConfig{
		RefreshIntervalVMs:  50 * time.Millisecond,
		RefreshIntervalHost: 0,
	}

	// A different task type in queued status should NOT block vm_inventory
	store.Create(&jobstore.JobRecord{
		JobID:    "other-job-1",
		TaskType: "hardware_inventory",
		Status:   jobstore.StatusQueued,
	})

	s := NewScheduler(store, cfg, schedulerTestLogger(), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	s.Run(ctx)

	// A new vm_inventory job should have been created
	vmJobs := store.jobsByType("vm_inventory")
	assert.GreaterOrEqual(t, len(vmJobs), 1)
}

func TestScheduler_HeartbeatIndependentOfHostInterval(t *testing.T) {
	store := newSchedulerMockStore()

	// Host inventory disabled entirely; heartbeat must still be scheduled.
	s := NewScheduler(store, SchedulerConfig{RefreshIntervalHost: 0}, schedulerTestLogger(), nil)

	specByType := map[string]refreshSpec{}
	for _, spec := range s.buildSpecs() {
		specByType[spec.taskType] = spec
	}

	// agent_status falls back to the default heartbeat, not the (zero) host interval.
	assert.Equal(t, defaultHeartbeatInterval, specByType["agent_status"].interval)
	assert.Equal(t, time.Duration(0), specByType["hardware_inventory"].interval)

	// An explicit heartbeat_interval wins.
	s2 := NewScheduler(store, SchedulerConfig{
		RefreshIntervalHost: 10 * time.Minute,
		HeartbeatInterval:   30 * time.Second,
	}, schedulerTestLogger(), nil)
	got := map[string]refreshSpec{}
	for _, spec := range s2.buildSpecs() {
		got[spec.taskType] = spec
	}
	assert.Equal(t, 30*time.Second, got["agent_status"].interval)
	assert.Equal(t, 10*time.Minute, got["hardware_inventory"].interval)
}

func TestPeriodicRefreshTaskID_RoundTrips(t *testing.T) {
	id := PeriodicRefreshTaskID("vm_inventory")
	assert.True(t, IsPeriodicRefreshTaskID(id), "scheduler-made id must be recognized: %q", id)
	assert.Contains(t, id, "refresh-vm_inventory-")

	// A backend-issued task id (opaque, "task_...") must NOT match.
	assert.False(t, IsPeriodicRefreshTaskID("task_9f8e7d6c"))
	assert.False(t, IsPeriodicRefreshTaskID(""))
}
