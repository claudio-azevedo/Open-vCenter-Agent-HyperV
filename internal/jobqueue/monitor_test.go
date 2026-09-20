package jobqueue

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ovc-agent/internal/jobstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockStore implements jobstore.Store for testing the monitor.
type mockStore struct {
	mu      sync.Mutex
	jobs    map[string]*jobstore.JobRecord
	updates []statusUpdate
}

type statusUpdate struct {
	JobID  string
	From   string
	To     string
	Fields map[string]interface{}
}

func newMockStore() *mockStore {
	return &mockStore{
		jobs: make(map[string]*jobstore.JobRecord),
	}
}

func (s *mockStore) Create(job *jobstore.JobRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.JobID] = job
	return nil
}

func (s *mockStore) GetByID(jobID string) (*jobstore.JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, os.ErrNotExist
	}
	return job, nil
}

func (s *mockStore) UpdateStatus(jobID string, from, to string, fields map[string]interface{}) error {
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
	s.updates = append(s.updates, statusUpdate{JobID: jobID, From: from, To: to, Fields: fields})
	return nil
}

func (s *mockStore) ListByStatus(_ string) ([]*jobstore.JobRecord, error) {
	return nil, nil
}

func (s *mockStore) ListRunningByVM(_ string) ([]*jobstore.JobRecord, error) {
	return nil, nil
}

func (s *mockStore) HasConflict(_, _, _ string) (bool, error) {
	return false, nil
}

func (s *mockStore) DeleteOlderThan(_ time.Duration, _ []string) (int, error) {
	return 0, nil
}

func (s *mockStore) RecoverOrphanedJobs(_ *slog.Logger) error {
	return nil
}

func (s *mockStore) Close() error {
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestMonitor_RunningCount(t *testing.T) {
	store := newMockStore()
	m := NewMonitor(store, testLogger(), 60*time.Second, nil)

	assert.Equal(t, 0, m.RunningCount())

	// Manually add entries to workers map for counting test
	m.mu.Lock()
	m.workers["job-1"] = &WorkerEntry{JobID: "job-1", Class: ClassRead}
	m.workers["job-2"] = &WorkerEntry{JobID: "job-2", Class: ClassWrite}
	m.mu.Unlock()

	assert.Equal(t, 2, m.RunningCount())
}

func TestMonitor_HasRunningHostWrite(t *testing.T) {
	store := newMockStore()
	m := NewMonitor(store, testLogger(), 60*time.Second, nil)

	assert.False(t, m.HasRunningHostWrite())

	m.mu.Lock()
	m.workers["job-1"] = &WorkerEntry{JobID: "job-1", Class: ClassRead}
	m.workers["job-2"] = &WorkerEntry{JobID: "job-2", Class: ClassWrite}
	m.mu.Unlock()

	assert.False(t, m.HasRunningHostWrite())

	m.mu.Lock()
	m.workers["job-3"] = &WorkerEntry{JobID: "job-3", Class: ClassHostWrite}
	m.mu.Unlock()

	assert.True(t, m.HasRunningHostWrite())
}

func TestMonitor_HasRunningWrite(t *testing.T) {
	store := newMockStore()
	m := NewMonitor(store, testLogger(), 60*time.Second, nil)

	assert.False(t, m.HasRunningWrite())

	m.mu.Lock()
	m.workers["job-1"] = &WorkerEntry{JobID: "job-1", Class: ClassRead}
	m.workers["job-2"] = &WorkerEntry{JobID: "job-2", Class: ClassDownload}
	m.mu.Unlock()

	assert.False(t, m.HasRunningWrite())

	m.mu.Lock()
	m.workers["job-3"] = &WorkerEntry{JobID: "job-3", Class: ClassWrite}
	m.mu.Unlock()

	assert.True(t, m.HasRunningWrite())

	// Also true for HostWrite
	m.mu.Lock()
	delete(m.workers, "job-3")
	m.workers["job-4"] = &WorkerEntry{JobID: "job-4", Class: ClassHostWrite}
	m.mu.Unlock()

	assert.True(t, m.HasRunningWrite())
}

func TestMonitor_HasRunningWriteForVM(t *testing.T) {
	store := newMockStore()
	m := NewMonitor(store, testLogger(), 60*time.Second, nil)

	assert.False(t, m.HasRunningWriteForVM("vm-abc"))

	m.mu.Lock()
	m.workers["job-1"] = &WorkerEntry{JobID: "job-1", Class: ClassWrite, VMIdentifier: "vm-xyz"}
	m.mu.Unlock()

	assert.False(t, m.HasRunningWriteForVM("vm-abc"))
	assert.True(t, m.HasRunningWriteForVM("vm-xyz"))

	// HostWrite should NOT match HasRunningWriteForVM (only ClassWrite)
	m.mu.Lock()
	m.workers["job-2"] = &WorkerEntry{JobID: "job-2", Class: ClassHostWrite, VMIdentifier: "vm-abc"}
	m.mu.Unlock()

	assert.False(t, m.HasRunningWriteForVM("vm-abc"))
}

func TestMonitor_Register_And_HandleTermination_MarksFailedIfStillRunning(t *testing.T) {
	store := newMockStore()
	var finishCalled atomic.Int32
	onFinish := func() { finishCalled.Add(1) }

	m := NewMonitor(store, testLogger(), 60*time.Second, onFinish)

	// Create a job in the store with status "running"
	store.Create(&jobstore.JobRecord{
		JobID:  "job-term-1",
		Status: jobstore.StatusRunning,
	})

	// Use a command that exits immediately (cross-platform: Go's own test binary trick won't work,
	// but on Windows "cmd /C exit 0" will work)
	cmd := exec.Command("cmd", "/C", "exit", "0")
	require.NoError(t, cmd.Start())

	entry := &WorkerEntry{
		JobID:        "job-term-1",
		PID:          cmd.Process.Pid,
		Process:      cmd.Process,
		Cmd:          cmd,
		StartedAt:    time.Now(),
		TaskType:     "vm_clone",
		VMIdentifier: "vm-123",
		Class:        ClassWrite,
	}

	m.Register(entry)

	// Wait for handleTermination to complete
	require.Eventually(t, func() bool {
		return m.RunningCount() == 0
	}, 5*time.Second, 50*time.Millisecond)

	// The job should be marked as failed since it was still "running" in DB
	job, err := store.GetByID("job-term-1")
	require.NoError(t, err)
	assert.Equal(t, jobstore.StatusFailed, job.Status)

	// onFinish should have been called
	assert.Equal(t, int32(1), finishCalled.Load())
}

func TestMonitor_Register_DoesNotMarkFailed_IfAlreadyCompleted(t *testing.T) {
	store := newMockStore()
	var finishCalled atomic.Int32
	onFinish := func() { finishCalled.Add(1) }

	m := NewMonitor(store, testLogger(), 60*time.Second, onFinish)

	// Create a job that is already "completed" in the store
	store.Create(&jobstore.JobRecord{
		JobID:  "job-term-2",
		Status: jobstore.StatusCompleted,
	})

	cmd := exec.Command("cmd", "/C", "exit", "0")
	require.NoError(t, cmd.Start())

	entry := &WorkerEntry{
		JobID:     "job-term-2",
		PID:       cmd.Process.Pid,
		Process:   cmd.Process,
		Cmd:       cmd,
		StartedAt: time.Now(),
		TaskType:  "vm_inventory",
		Class:     ClassRead,
	}

	m.Register(entry)

	// Wait for handleTermination to complete
	require.Eventually(t, func() bool {
		return m.RunningCount() == 0
	}, 5*time.Second, 50*time.Millisecond)

	// Job should still be "completed" - not overwritten
	job, err := store.GetByID("job-term-2")
	require.NoError(t, err)
	assert.Equal(t, jobstore.StatusCompleted, job.Status)

	// onFinish should still be called
	assert.Equal(t, int32(1), finishCalled.Load())
}

func TestMonitor_GetTimeoutForTask(t *testing.T) {
	store := newMockStore()
	defaultTimeout := 60 * time.Second
	m := NewMonitor(store, testLogger(), defaultTimeout, nil)

	tests := []struct {
		name     string
		taskType string
		action   string
		expected time.Duration
	}{
		{"vm_clone", "vm_clone", "", 1800 * time.Second},
		{"vm_inventory", "vm_inventory", "", 600 * time.Second},
		{"iso_inventory", "iso_inventory", "", 300 * time.Second},
		{"artifact_download", "artifact_download", "", 3600 * time.Second},
		{"vm_management vm_batch_start", "vm_management", "vm_batch_start", 1800 * time.Second},
		{"vm_management vm_batch_stop", "vm_management", "vm_batch_stop", 1800 * time.Second},
		{"vm_management vm_move", "vm_management", "vm_move", 1800 * time.Second},
		{"vm_management vm_rename", "vm_management", "vm_rename", 1800 * time.Second},
		{"vm_management vm_export_template", "vm_management", "vm_export_template", 1800 * time.Second},
		{"vm_management vm_shutdown", "vm_management", "vm_shutdown", 600 * time.Second},
		{"host_management suspend_drain", "host_management", "suspend_drain", 600 * time.Second},
		{"vm_management vm_start (default)", "vm_management", "vm_start", 60 * time.Second},
		{"vm_create", "vm_create", "", 1800 * time.Second},
		{"vm_edit", "vm_edit", "", 1800 * time.Second},
		{"unknown (default)", "unknown_type", "", 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := m.getTimeoutForTask(tt.taskType, tt.action)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMonitor_CheckTimeouts_KillsExpiredWorker(t *testing.T) {
	store := newMockStore()
	m := NewMonitor(store, testLogger(), 1*time.Millisecond, nil) // very short default timeout

	// Create a job in store
	store.Create(&jobstore.JobRecord{
		JobID:  "job-timeout-1",
		Status: jobstore.StatusRunning,
	})

	// Start a long-running process (ping with wait)
	cmd := exec.Command("cmd", "/C", "ping", "127.0.0.1", "-n", "60")
	require.NoError(t, cmd.Start())

	entry := &WorkerEntry{
		JobID:     "job-timeout-1",
		PID:       cmd.Process.Pid,
		Process:   cmd.Process,
		Cmd:       cmd,
		StartedAt: time.Now().Add(-2 * time.Second), // started 2s ago, timeout is 1ms
		TaskType:  "vm_create",
		Class:     ClassWrite,
	}

	m.mu.Lock()
	m.workers[entry.JobID] = entry
	m.mu.Unlock()

	// Also start the wait goroutine to handle cleanup
	go func() {
		_ = cmd.Wait()
		m.handleTermination(entry)
	}()

	// Run checkTimeouts - should kill the worker
	m.checkTimeouts()

	// Wait for the process to be terminated and cleaned up
	require.Eventually(t, func() bool {
		return m.RunningCount() == 0
	}, 5*time.Second, 50*time.Millisecond)

	// Job should be marked as failed
	job, err := store.GetByID("job-timeout-1")
	require.NoError(t, err)
	assert.Equal(t, jobstore.StatusFailed, job.Status)
}

func TestMonitor_Run_RespectsContextCancellation(t *testing.T) {
	store := newMockStore()
	m := NewMonitor(store, testLogger(), 60*time.Second, nil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Cancel the context
	cancel()

	// Run should exit promptly
	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Monitor.Run did not exit after context cancellation")
	}
}
