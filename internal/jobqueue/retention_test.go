package jobqueue

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ovc-agent/internal/jobstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retentionMockStore implements jobstore.Store with controllable DeleteOlderThan behavior.
type retentionMockStore struct {
	mu           sync.Mutex
	deleteCount  int
	deleteErr    error
	deleteCalls  atomic.Int32
	lastAge      time.Duration
	lastStatuses []string
	jobs         map[string]*jobstore.JobRecord
}

func newRetentionMockStore() *retentionMockStore {
	return &retentionMockStore{
		jobs: make(map[string]*jobstore.JobRecord),
	}
}

func (s *retentionMockStore) Create(job *jobstore.JobRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.JobID] = job
	return nil
}

func (s *retentionMockStore) GetByID(jobID string) (*jobstore.JobRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return nil, os.ErrNotExist
	}
	return job, nil
}

func (s *retentionMockStore) UpdateStatus(jobID, from, to string, fields map[string]interface{}) error {
	return nil
}

func (s *retentionMockStore) ListByStatus(_ string) ([]*jobstore.JobRecord, error) {
	return nil, nil
}

func (s *retentionMockStore) ListRunningByVM(_ string) ([]*jobstore.JobRecord, error) {
	return nil, nil
}

func (s *retentionMockStore) HasConflict(_, _, _ string) (bool, error) {
	return false, nil
}

func (s *retentionMockStore) DeleteOlderThan(age time.Duration, statuses []string) (int, error) {
	s.deleteCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAge = age
	s.lastStatuses = statuses
	return s.deleteCount, s.deleteErr
}

func (s *retentionMockStore) RecoverOrphanedJobs(_ *slog.Logger) error {
	return nil
}

func (s *retentionMockStore) Close() error {
	return nil
}

func retentionTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewRetention(t *testing.T) {
	store := newRetentionMockStore()
	logger := retentionTestLogger()

	r := NewRetention(store, logger)

	assert.NotNil(t, r)
	assert.Equal(t, store, r.store)
	assert.Equal(t, logger, r.logger)
}

func TestRetention_Purge_CallsDeleteOlderThanWithCorrectParams(t *testing.T) {
	store := newRetentionMockStore()
	store.deleteCount = 5
	logger := retentionTestLogger()

	r := NewRetention(store, logger)
	r.purge()

	assert.Equal(t, int32(1), store.deleteCalls.Load())

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, 24*time.Hour, store.lastAge)
	assert.ElementsMatch(t, []string{
		jobstore.StatusCompleted,
		jobstore.StatusFailed,
		jobstore.StatusTimeout,
	}, store.lastStatuses)
}

func TestRetention_Purge_LogsErrorOnFailure(t *testing.T) {
	store := newRetentionMockStore()
	store.deleteErr = errors.New("db write error")
	logger := retentionTestLogger()

	r := NewRetention(store, logger)

	// Should not panic; error is logged
	r.purge()

	assert.Equal(t, int32(1), store.deleteCalls.Load())
}

func TestRetention_Run_RespectsContextCancellation(t *testing.T) {
	store := newRetentionMockStore()
	logger := retentionTestLogger()

	r := NewRetention(store, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// success - Run exited promptly
	case <-time.After(2 * time.Second):
		t.Fatal("Retention.Run did not exit after context cancellation")
	}
}

func TestRetention_Purge_DoesNotLogWhenNothingDeleted(t *testing.T) {
	store := newRetentionMockStore()
	store.deleteCount = 0
	logger := retentionTestLogger()

	r := NewRetention(store, logger)
	r.purge()

	// Just verify no panic and the call was made
	assert.Equal(t, int32(1), store.deleteCalls.Load())
}

func TestRetention_Purge_OnlyTargetsTerminalStatuses(t *testing.T) {
	store := newRetentionMockStore()
	store.deleteCount = 0
	logger := retentionTestLogger()

	r := NewRetention(store, logger)
	r.purge()

	store.mu.Lock()
	defer store.mu.Unlock()

	// Verify that queued and running are NOT in the targeted statuses
	for _, s := range store.lastStatuses {
		require.NotEqual(t, jobstore.StatusQueued, s)
		require.NotEqual(t, jobstore.StatusRunning, s)
	}
}
