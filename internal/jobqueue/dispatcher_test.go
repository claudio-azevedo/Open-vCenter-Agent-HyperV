package jobqueue

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"ovc-agent/internal/jobstore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Dispatcher-specific Mock Store
// ---------------------------------------------------------------------------

// dispatcherMockStore implements jobstore.Store with controlled behavior for dispatcher tests.
// It differs from the monitor's mockStore by supporting a queuedJobs slice returned by
// ListByStatus("queued") in FIFO order.
type dispatcherMockStore struct {
	mu         sync.Mutex
	queuedJobs []*jobstore.JobRecord
}

func newDispatcherMockStore() *dispatcherMockStore {
	return &dispatcherMockStore{}
}

func (m *dispatcherMockStore) setQueuedJobs(jobs []*jobstore.JobRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queuedJobs = jobs
}

func (m *dispatcherMockStore) Create(_ *jobstore.JobRecord) error {
	return nil
}

func (m *dispatcherMockStore) GetByID(jobID string) (*jobstore.JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.queuedJobs {
		if j.JobID == jobID {
			return j, nil
		}
	}
	return nil, nil
}

func (m *dispatcherMockStore) UpdateStatus(_ string, _, _ string, _ map[string]interface{}) error {
	return nil
}

func (m *dispatcherMockStore) ListByStatus(status string) ([]*jobstore.JobRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if status == jobstore.StatusQueued {
		result := make([]*jobstore.JobRecord, len(m.queuedJobs))
		copy(result, m.queuedJobs)
		return result, nil
	}
	return nil, nil
}

func (m *dispatcherMockStore) ListRunningByVM(_ string) ([]*jobstore.JobRecord, error) {
	return nil, nil
}

func (m *dispatcherMockStore) HasConflict(_, _, _ string) (bool, error) {
	return false, nil
}

func (m *dispatcherMockStore) DeleteOlderThan(_ time.Duration, _ []string) (int, error) {
	return 0, nil
}

func (m *dispatcherMockStore) RecoverOrphanedJobs(_ *slog.Logger) error {
	return nil
}

func (m *dispatcherMockStore) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// newTestDispatcher creates a Dispatcher with a mock store and a spawnFunc that records dispatched job IDs.
func newTestDispatcher(store jobstore.Store, monitor *Monitor, maxConcurrent int) (*Dispatcher, *[]string) {
	dispatched := &[]string{}
	logger := testLogger()

	d := &Dispatcher{
		store:             store,
		monitor:           monitor,
		logger:            logger,
		maxConcurrentJobs: maxConcurrent,
		triggerCh:         make(chan struct{}, 1),
		spawnFunc: func(job *jobstore.JobRecord) error {
			*dispatched = append(*dispatched, job.JobID)
			return nil
		},
	}
	return d, dispatched
}

// newTestMonitorForDispatcher creates a Monitor suitable for dispatcher testing.
func newTestMonitorForDispatcher() *Monitor {
	logger := testLogger()
	return &Monitor{
		workers:        make(map[string]*WorkerEntry),
		store:          newMockStore(),
		logger:         logger,
		onFinish:       func() {},
		defaultTimeout: 60 * time.Second,
	}
}

// registerFakeWorker adds a fake worker entry to the monitor to simulate a running job.
func registerFakeWorker(m *Monitor, jobID, taskType, action, vmID string, class JobClass) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers[jobID] = &WorkerEntry{
		JobID:        jobID,
		PID:          99999,
		StartedAt:    time.Now(),
		TaskType:     taskType,
		Action:       action,
		VMIdentifier: vmID,
		Class:        class,
	}
}

// makeJob creates a JobRecord for testing with the given parameters.
func makeJob(id, taskType, action, vmID string) *jobstore.JobRecord {
	return &jobstore.JobRecord{
		JobID:        id,
		TaskID:       "task-" + id,
		TaskType:     taskType,
		Action:       action,
		VMIdentifier: vmID,
		Status:       jobstore.StatusQueued,
		CreatedAt:    time.Now().UTC(),
	}
}

// ---------------------------------------------------------------------------
// Tests: Max Concurrent Limit
// ---------------------------------------------------------------------------

func TestDispatcher_MaxConcurrentLimitPreventsDispatch(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// Register 2 running workers to fill the max concurrent slots (max=2)
	registerFakeWorker(monitor, "running-1", "vm_inventory", "", "", ClassRead)
	registerFakeWorker(monitor, "running-2", "vm_inventory", "", "", ClassRead)

	d, dispatched := newTestDispatcher(store, monitor, 2)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("queued-1", "vm_inventory", "", ""),
	})

	d.EvaluateAndDispatch()

	assert.Empty(t, *dispatched, "no jobs should be dispatched when max concurrent limit is reached")
}

func TestDispatcher_MaxConcurrentAllowsDispatchWhenUnderLimit(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// 1 running worker, max=3 → 2 slots available
	registerFakeWorker(monitor, "running-1", "vm_inventory", "", "", ClassRead)

	d, dispatched := newTestDispatcher(store, monitor, 3)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("queued-1", "vm_inventory", "", ""),
		makeJob("queued-2", "vm_inventory", "", ""),
	})

	d.EvaluateAndDispatch()

	assert.Len(t, *dispatched, 2)
	assert.Contains(t, *dispatched, "queued-1")
	assert.Contains(t, *dispatched, "queued-2")
}

func TestDispatcher_MaxConcurrentPartialDispatch(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// 2 running workers, max=3 → only 1 slot available
	registerFakeWorker(monitor, "running-1", "vm_inventory", "", "", ClassRead)
	registerFakeWorker(monitor, "running-2", "vm_inventory", "", "", ClassRead)

	d, dispatched := newTestDispatcher(store, monitor, 3)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("q1", "vm_inventory", "", ""),
		makeJob("q2", "vm_inventory", "", ""),
		makeJob("q3", "vm_inventory", "", ""),
	})

	d.EvaluateAndDispatch()

	// Only 1 should be dispatched (first in FIFO)
	require.Len(t, *dispatched, 1)
	assert.Equal(t, "q1", (*dispatched)[0])
}

// ---------------------------------------------------------------------------
// Tests: VM Mutual Exclusion
// ---------------------------------------------------------------------------

func TestDispatcher_VMMutualExclusionBlocksSecondWriteForSameVM(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// A write job is already running for "vm-A"
	registerFakeWorker(monitor, "running-write", "vm_edit", "", "vm-A", ClassWrite)

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("queued-same-vm", "vm_clone", "", "vm-A"),
		makeJob("queued-diff-vm", "vm_edit", "", "vm-B"),
	})

	d.EvaluateAndDispatch()

	// Only the different-VM write should be dispatched
	assert.Len(t, *dispatched, 1)
	assert.Contains(t, *dispatched, "queued-diff-vm")
	assert.NotContains(t, *dispatched, "queued-same-vm")
}

func TestDispatcher_VMMutualExclusionAllowsWriteForDifferentVMs(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("write-vm-A", "vm_edit", "", "vm-A"),
		makeJob("write-vm-B", "vm_clone", "", "vm-B"),
		makeJob("write-vm-C", "vm_management", "vm_start", "vm-C"),
	})

	d.EvaluateAndDispatch()

	// All should be dispatched since they target different VMs
	assert.Len(t, *dispatched, 3)
}

// ---------------------------------------------------------------------------
// Tests: Host Write Blocks All Other Writes
// ---------------------------------------------------------------------------

func TestDispatcher_HostWriteBlocksAllOtherWrites(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// A host write (suspend) is running
	registerFakeWorker(monitor, "running-host-write", "host_management", "suspend", "", ClassHostWrite)

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("queued-write", "vm_edit", "", "vm-A"),
		makeJob("queued-host-write", "host_management", "restart", ""),
		makeJob("queued-read", "vm_inventory", "", ""),
	})

	d.EvaluateAndDispatch()

	// Only reads should be dispatched when a host write is running
	assert.Len(t, *dispatched, 1)
	assert.Contains(t, *dispatched, "queued-read")
}

func TestDispatcher_HostWriteNotDispatchedWhileAnyWriteRunning(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// A regular write is running for vm-X
	registerFakeWorker(monitor, "running-write", "vm_edit", "", "vm-X", ClassWrite)

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("queued-host-write", "host_management", "suspend", ""),
		makeJob("queued-read", "vm_inventory", "", ""),
	})

	d.EvaluateAndDispatch()

	// Read should dispatch, host write should NOT
	assert.Contains(t, *dispatched, "queued-read")
	assert.NotContains(t, *dispatched, "queued-host-write")
}

// ---------------------------------------------------------------------------
// Tests: Reads Dispatch Freely Alongside Writes
// ---------------------------------------------------------------------------

func TestDispatcher_ReadsDispatchFreelyAlongsideWrites(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	registerFakeWorker(monitor, "running-write", "vm_edit", "", "vm-A", ClassWrite)

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("read-1", "vm_inventory", "", ""),
		makeJob("read-2", "iso_inventory", "", ""),
		makeJob("read-3", "vm_status", "", "vm-A"), // read for same VM as write - still OK
	})

	d.EvaluateAndDispatch()

	// All reads should be dispatched regardless of running writes
	assert.Len(t, *dispatched, 3)
	assert.Contains(t, *dispatched, "read-1")
	assert.Contains(t, *dispatched, "read-2")
	assert.Contains(t, *dispatched, "read-3")
}

func TestDispatcher_ReadsDispatchDuringHostWrite(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	registerFakeWorker(monitor, "running-host-write", "host_management", "suspend", "", ClassHostWrite)

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("read-1", "vm_inventory", "", ""),
		makeJob("read-2", "template_inventory", "", ""),
		makeJob("write-1", "vm_edit", "", "vm-A"), // should NOT dispatch
	})

	d.EvaluateAndDispatch()

	// Only reads should dispatch
	assert.Len(t, *dispatched, 2)
	assert.Contains(t, *dispatched, "read-1")
	assert.Contains(t, *dispatched, "read-2")
}

func TestDispatcher_HostManagementRefreshClassifiedAsRead(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// A host write is running - reads should still dispatch
	registerFakeWorker(monitor, "running-host-write", "host_management", "suspend", "", ClassHostWrite)

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("refresh-hw", "host_management", "refresh_hardware", ""),
		makeJob("refresh-inv", "host_management", "refresh_inventory", ""),
	})

	d.EvaluateAndDispatch()

	// Both should dispatch (they are classified as Read)
	assert.Len(t, *dispatched, 2)
}

// ---------------------------------------------------------------------------
// Tests: Drain Mode Blocks All Dispatch
// ---------------------------------------------------------------------------

func TestDispatcher_DrainModeBlocksAllDispatch(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	d, dispatched := newTestDispatcher(store, monitor, 10)

	// Enter drain mode
	d.mu.Lock()
	d.drainMode = true
	d.mu.Unlock()

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("queued-read", "vm_inventory", "", ""),
		makeJob("queued-write", "vm_edit", "", "vm-A"),
		makeJob("queued-download", "artifact_download", "", ""),
	})

	d.EvaluateAndDispatch()

	assert.Empty(t, *dispatched, "no jobs should be dispatched in drain mode")
}

func TestDispatcher_DrainModeExitResumesDispatch(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	d, dispatched := newTestDispatcher(store, monitor, 10)

	// Enter drain mode
	d.mu.Lock()
	d.drainMode = true
	d.mu.Unlock()

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("queued-1", "vm_inventory", "", ""),
	})

	d.EvaluateAndDispatch()
	assert.Empty(t, *dispatched)

	// Exit drain mode
	d.mu.Lock()
	d.drainMode = false
	d.mu.Unlock()

	d.EvaluateAndDispatch()
	assert.Len(t, *dispatched, 1)
	assert.Contains(t, *dispatched, "queued-1")
}

func TestDispatcher_InDrainMode(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()
	d, _ := newTestDispatcher(store, monitor, 10)

	assert.False(t, d.InDrainMode())

	d.mu.Lock()
	d.drainMode = true
	d.mu.Unlock()

	assert.True(t, d.InDrainMode())
}

// ---------------------------------------------------------------------------
// Tests: FIFO Ordering Respected
// ---------------------------------------------------------------------------

func TestDispatcher_FIFOOrderingRespected(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	d, dispatched := newTestDispatcher(store, monitor, 10)

	// Create jobs in a specific FIFO order (the store returns them in this order)
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	jobs := []*jobstore.JobRecord{
		{
			JobID:     "first",
			TaskID:    "t1",
			TaskType:  "vm_inventory",
			Status:    jobstore.StatusQueued,
			CreatedAt: baseTime,
		},
		{
			JobID:     "second",
			TaskID:    "t2",
			TaskType:  "vm_inventory",
			Status:    jobstore.StatusQueued,
			CreatedAt: baseTime.Add(1 * time.Minute),
		},
		{
			JobID:     "third",
			TaskID:    "t3",
			TaskType:  "vm_inventory",
			Status:    jobstore.StatusQueued,
			CreatedAt: baseTime.Add(2 * time.Minute),
		},
	}
	store.setQueuedJobs(jobs)

	d.EvaluateAndDispatch()

	// All should be dispatched and in FIFO order
	require.Len(t, *dispatched, 3)
	assert.Equal(t, "first", (*dispatched)[0])
	assert.Equal(t, "second", (*dispatched)[1])
	assert.Equal(t, "third", (*dispatched)[2])
}

func TestDispatcher_FIFOWithMixedClasses(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	d, dispatched := newTestDispatcher(store, monitor, 10)

	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	jobs := []*jobstore.JobRecord{
		{
			JobID:        "write-1",
			TaskID:       "t1",
			TaskType:     "vm_edit",
			VMIdentifier: "vm-A",
			Status:       jobstore.StatusQueued,
			CreatedAt:    baseTime,
		},
		{
			JobID:     "read-1",
			TaskID:    "t2",
			TaskType:  "vm_inventory",
			Status:    jobstore.StatusQueued,
			CreatedAt: baseTime.Add(1 * time.Minute),
		},
		{
			JobID:        "write-2",
			TaskID:       "t3",
			TaskType:     "vm_clone",
			VMIdentifier: "vm-B",
			Status:       jobstore.StatusQueued,
			CreatedAt:    baseTime.Add(2 * time.Minute),
		},
	}
	store.setQueuedJobs(jobs)

	d.EvaluateAndDispatch()

	// All should be dispatched in order (different VMs, no conflicts)
	require.Len(t, *dispatched, 3)
	assert.Equal(t, "write-1", (*dispatched)[0])
	assert.Equal(t, "read-1", (*dispatched)[1])
	assert.Equal(t, "write-2", (*dispatched)[2])
}

// ---------------------------------------------------------------------------
// Tests: Downloads
// ---------------------------------------------------------------------------

func TestDispatcher_DownloadJobsDispatchFreely(t *testing.T) {
	store := newDispatcherMockStore()
	monitor := newTestMonitorForDispatcher()

	// A write is running - downloads should still dispatch
	registerFakeWorker(monitor, "running-write", "vm_edit", "", "vm-A", ClassWrite)

	d, dispatched := newTestDispatcher(store, monitor, 10)

	store.setQueuedJobs([]*jobstore.JobRecord{
		makeJob("dl-1", "artifact_download", "", ""),
		makeJob("dl-2", "artifact_download", "", ""),
	})

	d.EvaluateAndDispatch()

	assert.Len(t, *dispatched, 2)
	assert.Contains(t, *dispatched, "dl-1")
	assert.Contains(t, *dispatched, "dl-2")
}
