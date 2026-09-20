package jobstore

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_jobs.db")
	store, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestJob(id, taskType, status string) *JobRecord {
	return &JobRecord{
		JobID:        id,
		TaskID:       "task-" + id,
		TaskType:     taskType,
		Status:       status,
		VMIdentifier: "vm-001",
		Payload:      []byte(`{"test": true}`),
		CreatedAt:    time.Now().UTC(),
	}
}

func TestCreateAndGet(t *testing.T) {
	store := openTestStore(t)

	job := newTestJob("job-1", "vm_management", StatusQueued)
	job.Action = "vm_start"
	require.NoError(t, store.Create(job))

	got, err := store.GetByID("job-1")
	require.NoError(t, err)
	assert.Equal(t, "vm_management", got.TaskType)
	assert.Equal(t, "vm_start", got.Action)
	assert.Equal(t, StatusQueued, got.Status)
	assert.Equal(t, "vm-001", got.VMIdentifier)
	assert.JSONEq(t, `{"test": true}`, string(got.Payload))
}

func TestCreateDuplicate(t *testing.T) {
	store := openTestStore(t)
	job := newTestJob("job-dup", "vm_edit", StatusQueued)
	require.NoError(t, store.Create(job))
	err := store.Create(job)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestCreateEmptyID(t *testing.T) {
	store := openTestStore(t)
	err := store.Create(newTestJob("", "vm_edit", StatusQueued))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "job ID is required")
}

func TestGetNotFound(t *testing.T) {
	store := openTestStore(t)
	_, err := store.GetByID("ghost")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateStatusCAS(t *testing.T) {
	store := openTestStore(t)
	require.NoError(t, store.Create(newTestJob("job-cas", "vm_edit", StatusQueued)))

	// wrong "from" is rejected, job unchanged
	err := store.UpdateStatus("job-cas", StatusRunning, StatusCompleted, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected")
	got, _ := store.GetByID("job-cas")
	assert.Equal(t, StatusQueued, got.Status)

	// valid transition with fields
	now := time.Now().UTC()
	require.NoError(t, store.UpdateStatus("job-cas", StatusQueued, StatusRunning, map[string]interface{}{
		"started_at": now,
		"worker_pid": 1234,
	}))
	got, _ = store.GetByID("job-cas")
	assert.Equal(t, StatusRunning, got.Status)
	assert.Equal(t, 1234, got.WorkerPID)
	require.NotNil(t, got.StartedAt)
	assert.WithinDuration(t, now, *got.StartedAt, time.Second)

	require.NoError(t, store.UpdateStatus("job-cas", StatusRunning, StatusFailed, map[string]interface{}{
		"completed_at":  time.Now().UTC(),
		"error_message": "boom",
	}))
	got, _ = store.GetByID("job-cas")
	assert.Equal(t, StatusFailed, got.Status)
	assert.Equal(t, "boom", got.ErrorMessage)
	require.NotNil(t, got.CompletedAt)
}

func TestUpdateStatusNotFound(t *testing.T) {
	store := openTestStore(t)
	err := store.UpdateStatus("ghost", StatusQueued, StatusRunning, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestListByStatusFIFO(t *testing.T) {
	store := openTestStore(t)
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, j := range []struct {
		id string
		at time.Time
	}{
		{"job-c", base.Add(2 * time.Minute)},
		{"job-a", base},
		{"job-b", base.Add(1 * time.Minute)},
	} {
		rec := newTestJob(j.id, "vm_inventory", StatusQueued)
		rec.CreatedAt = j.at
		require.NoError(t, store.Create(rec))
	}

	queued, err := store.ListByStatus(StatusQueued)
	require.NoError(t, err)
	require.Len(t, queued, 3)
	assert.Equal(t, []string{"job-a", "job-b", "job-c"},
		[]string{queued[0].JobID, queued[1].JobID, queued[2].JobID})

	empty, err := store.ListByStatus(StatusTimeout)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestListByStatusMovesWithUpdate(t *testing.T) {
	store := openTestStore(t)
	require.NoError(t, store.Create(newTestJob("job-x", "vm_edit", StatusQueued)))
	require.NoError(t, store.UpdateStatus("job-x", StatusQueued, StatusRunning, nil))

	q, _ := store.ListByStatus(StatusQueued)
	assert.Empty(t, q)
	r, _ := store.ListByStatus(StatusRunning)
	require.Len(t, r, 1)
	assert.Equal(t, "job-x", r[0].JobID)
}

func TestListRunningByVM(t *testing.T) {
	store := openTestStore(t)

	j1 := newTestJob("job-vm1", "vm_edit", StatusQueued)
	j1.VMIdentifier = "vm-test"
	require.NoError(t, store.Create(j1))
	require.NoError(t, store.UpdateStatus("job-vm1", StatusQueued, StatusRunning, nil))

	j2 := newTestJob("job-vm2", "vm_clone", StatusQueued)
	j2.VMIdentifier = "vm-test"
	require.NoError(t, store.Create(j2))

	running, err := store.ListRunningByVM("vm-test")
	require.NoError(t, err)
	require.Len(t, running, 1)
	assert.Equal(t, "job-vm1", running[0].JobID)
}

func TestHasConflict(t *testing.T) {
	store := openTestStore(t)

	j := newTestJob("job-c1", "vm_management", StatusQueued)
	j.Action = "vm_start"
	j.VMIdentifier = "vm-abc"
	require.NoError(t, store.Create(j))

	got, err := store.HasConflict("vm_management", "vm_start", "vm-abc")
	require.NoError(t, err)
	assert.True(t, got)

	got, _ = store.HasConflict("vm_management", "vm_stop", "vm-abc")
	assert.False(t, got, "different action, no conflict")

	got, _ = store.HasConflict("vm_management", "vm_start", "vm-xyz")
	assert.False(t, got, "different vm, no conflict")

	got, _ = store.HasConflict("vm_management", "vm_start", "")
	assert.False(t, got, "empty vm identifier short-circuits")

	// non-vm_management: type-only match, action ignored
	j2 := newTestJob("job-c2", "vm_edit", StatusQueued)
	j2.VMIdentifier = "vm-def"
	require.NoError(t, store.Create(j2))
	require.NoError(t, store.UpdateStatus("job-c2", StatusQueued, StatusRunning, nil))
	got, _ = store.HasConflict("vm_edit", "", "vm-def")
	assert.True(t, got)
	got, _ = store.HasConflict("vm_clone", "", "vm-def")
	assert.False(t, got)

	// completed jobs never conflict
	j3 := newTestJob("job-c3", "vm_edit", StatusCompleted)
	j3.VMIdentifier = "vm-ghi"
	require.NoError(t, store.Create(j3))
	got, _ = store.HasConflict("vm_edit", "", "vm-ghi")
	assert.False(t, got)
}

func TestDeleteOlderThan(t *testing.T) {
	store := openTestStore(t)
	old := time.Now().UTC().Add(-48 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Hour)

	mk := func(id, status string, at time.Time) {
		r := newTestJob(id, "vm_edit", status)
		r.CreatedAt = at
		require.NoError(t, store.Create(r))
	}
	mk("d1", StatusCompleted, old)
	mk("d2", StatusFailed, old)
	mk("d3", StatusCompleted, recent)
	mk("d4", StatusQueued, old) // never purged regardless of age

	n, err := store.DeleteOlderThan(24*time.Hour, []string{StatusCompleted, StatusFailed, StatusTimeout})
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	_, err = store.GetByID("d3")
	assert.NoError(t, err)
	_, err = store.GetByID("d4")
	assert.NoError(t, err)

	n, err = store.DeleteOlderThan(24*time.Hour, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestRecoverOrphanedJobs(t *testing.T) {
	store := openTestStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// dead PID -> failed
	require.NoError(t, store.Create(newTestJob("orphan-dead", "vm_edit", StatusQueued)))
	require.NoError(t, store.UpdateStatus("orphan-dead", StatusQueued, StatusRunning, map[string]interface{}{
		"worker_pid": 999999999,
	}))
	// no PID -> failed
	require.NoError(t, store.Create(newTestJob("orphan-nopid", "vm_clone", StatusQueued)))
	require.NoError(t, store.UpdateStatus("orphan-nopid", StatusQueued, StatusRunning, nil))
	// alive PID (this test process) -> untouched
	require.NoError(t, store.Create(newTestJob("orphan-alive", "vm_edit", StatusQueued)))
	require.NoError(t, store.UpdateStatus("orphan-alive", StatusQueued, StatusRunning, map[string]interface{}{
		"worker_pid": os.Getpid(),
	}))

	require.NoError(t, store.RecoverOrphanedJobs(logger))

	dead, _ := store.GetByID("orphan-dead")
	assert.Equal(t, StatusFailed, dead.Status)
	assert.Contains(t, dead.ErrorMessage, "orphaned job")

	nopid, _ := store.GetByID("orphan-nopid")
	assert.Equal(t, StatusFailed, nopid.Status)
	assert.Contains(t, nopid.ErrorMessage, "no worker PID")

	alive, _ := store.GetByID("orphan-alive")
	assert.Equal(t, StatusRunning, alive.Status)
}

// TestConcurrentAccess simulates the service and a worker subprocess hammering
// the same DB file through separate handles.
func TestConcurrentAccess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")

	svc, err := Open(dbPath)
	require.NoError(t, err)
	defer svc.Close()

	require.NoError(t, svc.Create(newTestJob("shared-1", "vm_management", StatusQueued)))

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			h, err := Open(dbPath)
			if err != nil {
				errs <- err
				return
			}
			defer h.Close()
			if err := h.Create(newTestJob(fmt.Sprintf("w-%d", n), "vm_inventory", StatusQueued)); err != nil {
				errs <- err
			}
			if _, err := h.ListByStatus(StatusQueued); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent op failed: %v", e)
	}

	// the CAS transition must be won exactly once across contenders
	var winners int
	var mu sync.Mutex
	var wg2 sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			h, err := Open(dbPath)
			if err != nil {
				return
			}
			defer h.Close()
			if err := h.UpdateStatus("shared-1", StatusQueued, StatusRunning, nil); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg2.Wait()
	assert.Equal(t, 1, winners, "exactly one CAS winner")
}
