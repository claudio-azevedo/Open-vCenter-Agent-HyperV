package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"ovc-agent/internal/config"
	"ovc-agent/internal/hyperv"
	"ovc-agent/internal/jobqueue"
	"ovc-agent/internal/jobstore"
	"ovc-agent/internal/preflight"
	"ovc-agent/internal/protocol"
	"ovc-agent/internal/rabbitmq"
	"ovc-agent/internal/service"
	"ovc-agent/internal/tasks"
	"ovc-agent/internal/upgrade"
	"ovc-agent/internal/worker"
)

func main() {
	if len(os.Args) > 1 {
		handleCommand(os.Args[1])
		return
	}

	// Default: if running as Windows service, run as service; otherwise run interactively
	if service.IsWindowsService() {
		logger := setupLogger("info", config.DefaultLogFile())
		if err := service.RunAsService(logger, runAgent); err != nil {
			fmt.Fprintf(os.Stderr, "Service error: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Running interactively (e.g., from command line)
		runInteractive()
	}
}

func handleCommand(cmd string) {
	switch cmd {
	case "install":
		if err := service.InstallService(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to install service: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Service installed successfully.")

	case "uninstall":
		if err := service.UninstallService(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to uninstall service: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Service uninstalled successfully.")

	case "start":
		if err := service.StartService(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start service: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Service started successfully.")

	case "stop":
		if err := service.StopService(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to stop service: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Service stopped successfully.")

	case "run":
		// Explicit run mode (used by service manager)
		if service.IsWindowsService() {
			logger := setupLogger("info", config.DefaultLogFile())
			if err := service.RunAsService(logger, runAgent); err != nil {
				fmt.Fprintf(os.Stderr, "Service error: %v\n", err)
				os.Exit(1)
			}
		} else {
			runInteractive()
		}

	case "upgrade":
		// Rotate log before upgrade to ensure clean log for startup verification
		rotateLogFile(config.DefaultLogFile())
		logger := setupLogger("info", config.DefaultLogFile())
		logger.Info("upgrade mode initiated", "version", tasks.AgentVersion)

		// Capture the pending response (task_id, artifact_id, old_version) BEFORE running
		// the upgrade, so we can report a failure to the backend if the upgrade fails and
		// rolls back (Run removes the pending file on rollback).
		pending := upgrade.PeekPendingResponse(upgrade.GetAgentDir())

		if err := upgrade.Run(logger); err != nil {
			logger.Error("upgrade failed", "error", err)
			publishUpgradeFailure(logger, pending, err)
			fmt.Fprintf(os.Stderr, "Upgrade failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Upgrade completed successfully.")

	case "version":
		fmt.Printf("Open vCenter Agent v%s\n", tasks.AgentVersion)

	case "job":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: ovc-agent.exe job <job_id>\n")
			os.Exit(1)
		}
		worker.RunJob(os.Args[2])

	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		fmt.Println("Usage: ovc-agent.exe [install|uninstall|start|stop|run|upgrade|job|version]")
		os.Exit(1)
	}
}

func runInteractive() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		cancel()
	}()

	if err := runAgent(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Agent error: %v\n", err)
		os.Exit(1)
	}
}

func runAgent(ctx context.Context) error {
	// Load configuration
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("failed to load config: %v", err)
	}

	// Setup logger
	logger := setupLogger(cfg.LogLevel, cfg.LogFile)

	// Cleanup leftover upgrade artifacts (.bak, uuid binaries, .failed)
	upgrade.Cleanup(logger)

	// Setup PowerShell error log in same directory as main log
	hyperv.SetErrorLogDir(filepath.Dir(cfg.LogFile))

	// Enable PowerShell debug log when log_level=debug
	if cfg.LogLevel == "debug" {
		hyperv.EnableDebugLog(filepath.Dir(cfg.LogFile))
	}

	logger.Info("starting ovc-agent",
		"version", tasks.AgentVersion,
		"host_id", cfg.HostID,
		"request_queue", cfg.RequestQueue(),
		"response_queue", cfg.ResponseQueue(),
	)

	// Refuse to start on a host without the Hyper-V role. Writes agent_failed.log
	// next to the binary and exits non-zero.
	if err := preflight.EnsureHyperV(ctx); err != nil {
		preflight.WriteFailure(config.AgentDir(), err)
		logger.Error("preflight failed: Hyper-V role not available", "error", err)
		return fmt.Errorf("preflight failed: %w", err)
	}

	// Open job store (SharedStore opens/closes DB per operation to allow worker processes
	// to access the same file without lock contention)
	store, err := jobstore.OpenShared(config.JobStorePath())
	if err != nil {
		return fmt.Errorf("failed to open job store: %v", err)
	}
	defer store.Close()

	// Connect to RabbitMQ
	conn := rabbitmq.NewConnection(cfg.RabbitMQURL, logger)
	if err := conn.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %v", err)
	}
	defer conn.Close()

	// Create publisher and consumer
	publisher := rabbitmq.NewPublisher(conn, cfg.ResponseQueue(), logger)
	consumer := rabbitmq.NewConsumer(conn, cfg.RequestQueue(), logger)

	// Declare response queue
	if err := publisher.DeclareQueue(); err != nil {
		return fmt.Errorf("failed to declare response queue: %v", err)
	}

	// Declare the last-value inventory queues (x-max-length=1), matching
	// ovc-backend's queues.py layout.
	for _, kind := range []string{"agent_status", "vm_inventory", "host_inventory", "template_inventory", "iso_inventory"} {
		if err := publisher.DeclareStateQueue(cfg.StateQueue(kind)); err != nil {
			return fmt.Errorf("failed to declare state queue '%s': %v", kind, err)
		}
	}

	// Declare the quick-metrics queues as plain durable queues (NO x-max-length -
	// they are a short time-series, not last-value), matching ovc-backend.
	for _, kind := range []string{"vm_metrics", "host_metrics"} {
		if err := publisher.DeclarePlainQueue(cfg.StateQueue(kind)); err != nil {
			return fmt.Errorf("failed to declare metrics queue '%s': %v", kind, err)
		}
	}

	// Create job queue components
	taskTimeout := time.Duration(cfg.TaskTimeout) * time.Second

	// Create monitor with onFinish callback that triggers dispatcher re-evaluation
	var queueDispatcher *jobqueue.Dispatcher
	monitor := jobqueue.NewMonitor(store, logger, taskTimeout, func() {
		if queueDispatcher != nil {
			queueDispatcher.TriggerEvaluation()
		}
	})

	queueDispatcher = jobqueue.NewDispatcher(store, monitor, cfg.MaxConcurrentJobs, logger)

	// Wire onJobCompleted callback to detect agent upgrade completions.
	// When a host_update_agent worker finishes staging the new binary, enter
	// drain mode: wait for other workers to finish, then launch the upgrade.
	monitor.SetOnJobCompleted(func(entry *jobqueue.WorkerEntry) {
		if entry.TaskType != string(tasks.TaskAgentUpgrade) {
			return
		}

		job, err := store.GetByID(entry.JobID)
		if err != nil {
			logger.Error("onJobCompleted: failed to read job for upgrade check",
				"job_id", entry.JobID, "error", err)
			return
		}

		var p tasks.AgentUpgradePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil || p.Version == "" {
			logger.Warn("onJobCompleted: could not read agent upgrade version",
				"job_id", entry.JobID, "error", err)
			return
		}

		binaryPath := filepath.Join(upgrade.GetAgentDir(),
			fmt.Sprintf("ovc-agent-%s.exe", p.Version))

		logger.Info("upgrade: binary staged, entering drain mode",
			"job_id", entry.JobID, "binary_path", binaryPath)

		queueDispatcher.EnterDrainMode(binaryPath)
	})

	// Wire onWorkerFailed callback to publish failure responses to the backend
	// when a worker is killed by timeout or terminates unexpectedly.
	monitor.SetOnWorkerFailed(func(entry *jobqueue.WorkerEntry, reason string) {
		errMsg := fmt.Sprintf("task failed: %s", reason)
		response := &tasks.TaskResponse{
			TaskID:    entry.TaskID,
			HostID:    cfg.HostID,
			Type:      tasks.TaskType(entry.TaskType),
			Status:    "failed",
			Error:     &errMsg,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
		if err := publisher.Publish(ctx, response); err != nil {
			logger.Error("onWorkerFailed: failed to publish failure response",
				"job_id", entry.JobID, "task_id", entry.TaskID, "error", err)
		} else {
			logger.Info("onWorkerFailed: published failure response to backend",
				"job_id", entry.JobID, "task_id", entry.TaskID, "task_type", entry.TaskType)
		}
	})

	schedulerCfg := jobqueue.SchedulerConfig{
		RefreshIntervalVMs:  time.Duration(cfg.RefreshIntervalVMs) * time.Second,
		RefreshIntervalHost: time.Duration(cfg.RefreshIntervalHost) * time.Second,
		MetricsInterval:     time.Duration(cfg.MetricsInterval) * time.Second,
		HeartbeatInterval:   time.Duration(cfg.HeartbeatInterval) * time.Second,
	}
	scheduler := jobqueue.NewScheduler(store, schedulerCfg, logger, queueDispatcher.TriggerEvaluation)

	retention := jobqueue.NewRetention(store, logger)

	// Recover orphaned jobs from previous run (mark running→failed for dead workers)
	if err := store.RecoverOrphanedJobs(logger); err != nil {
		logger.Error("failed to recover orphaned jobs", "error", err)
	}

	// Trigger dispatcher to process any queued jobs left from previous run
	queueDispatcher.TriggerEvaluation()

	// Start background goroutines
	go monitor.Run(ctx)
	go scheduler.Run(ctx)
	go retention.Run(ctx)
	go queueDispatcher.RunLoop(ctx.Done())

	// Create a minimal task dispatcher for the synchronous startup agent_status
	startupDispatcher := tasks.NewDispatcher(cfg.HostID, taskTimeout, cfg.TemplatePath, cfg.LocalISOPath, cfg.AdditionalVMStorage, logger)
	startupDispatcher.SetRefreshIntervals(cfg.RefreshIntervalVMs, cfg.RefreshIntervalHost)
	startupDispatcher.DetectCluster(ctx)

	// Check for pending upgrade response (written by the previous agent before upgrade).
	// If found, publish the "completed" response to confirm the upgrade succeeded.
	if pending := upgrade.ReadPendingResponse(upgrade.GetAgentDir()); pending != nil {
		logger.Info("upgrade: found pending response, publishing upgrade completion",
			"task_id", pending.TaskID, "version", pending.Version,
			"old_version", pending.OldVersion)

		upgradeResult := &tasks.TaskResponse{
			TaskID:   pending.TaskID,
			HostID:   cfg.HostID,
			Type:     tasks.TaskAgentUpgrade,
			Function: "host_update_agent",
			Status:   "completed",
			Result: map[string]interface{}{
				"version":     pending.Version,
				"size_bytes":  pending.SizeBytes,
				"upgraded":    true,
				"old_version": pending.OldVersion,
				"new_version": tasks.AgentVersion,
			},
			FinishedAt: time.Now().UTC().Format(time.RFC3339),
		}

		if err := publisher.Publish(ctx, upgradeResult); err != nil {
			logger.Error("upgrade: failed to publish completion response", "error", err)
		} else {
			logger.Info("upgrade: published completion response successfully",
				"task_id", pending.TaskID, "new_version", tasks.AgentVersion)
		}
	}

	// Preflight passed and we are about to serve - clear any stale failure marker.
	preflight.ClearFailure(config.AgentDir())

	// Publish agent_status on startup so the backend learns version, hostname,
	// hypervisor type, etc. immediately.
	publishAgentStatus(ctx, startupDispatcher, publisher, cfg, logger)

	// Start consumer. Run owns its own AMQP channel and resubscribes on its own
	// after a channel/connection drop, so the request queue keeps being
	// consumed without the main loop having to restart anything.
	deliveries := consumer.Run(ctx)

	// Handle reconnection: re-declare our queues (idempotent) so the agent works
	// even when the broker lost them (e.g. a non-durable broker restart).
	conn.OnReconnect(func() {
		logger.Info("reconnected, re-declaring queues")
		if err := publisher.DeclareQueue(); err != nil {
			logger.Error("failed to re-declare response queue", "error", err)
		}
		for _, kind := range []string{"agent_status", "vm_inventory", "host_inventory", "template_inventory", "iso_inventory"} {
			if err := publisher.DeclareStateQueue(cfg.StateQueue(kind)); err != nil {
				logger.Error("failed to re-declare state queue", "kind", kind, "error", err)
			}
		}
		for _, kind := range []string{"vm_metrics", "host_metrics"} {
			if err := publisher.DeclarePlainQueue(cfg.StateQueue(kind)); err != nil {
				logger.Error("failed to re-declare metrics queue", "kind", kind, "error", err)
			}
		}
	})

	// Periodic agent_status re-publishing is handled by the scheduler
	// (jobqueue.Scheduler) like the other inventories.

	// Main message processing loop - enqueue tasks via the job store
	logger.Info("agent ready, waiting for tasks")
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down agent")
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				// Run only closes its output when ctx is done; anything else
				// means the consumer gave up and the agent is deaf to tasks -
				// exit so the service manager restarts us.
				if ctx.Err() != nil {
					logger.Info("shutting down agent")
					return nil
				}
				return fmt.Errorf("task consumer stopped unexpectedly")
			}

			// Parse the AgentRequest (ovc-backend contract).
			var req protocol.AgentRequest
			if err := json.Unmarshal(delivery.Body, &req); err != nil || req.ID == "" || req.Function == "" {
				logger.Error("failed to parse agent request",
					"error", err, "body", string(delivery.Body))
				delivery.Nack(false, false) // Don't requeue malformed messages
				continue
			}

			taskType, action := tasks.ResolveFunction(req.Function)

			logger.Debug("received agent request",
				"task_id", req.ID, "function", req.Function,
				"type", taskType, "action", action)

			// Check if shutting down - if so, requeue the message for the next instance
			select {
			case <-ctx.Done():
				logger.Info("shutdown in progress, requeueing task", "task_id", req.ID)
				delivery.Nack(false, true)
				return nil
			default:
			}

			vmIdentifier := extractVMIdentifier(taskType, req.Params)

			// For write jobs: check for duplicate/conflict
			if isWriteTask(taskType) {
				conflict, _ := store.HasConflict(string(taskType), action, vmIdentifier)
				if conflict {
					publishConflictResponse(ctx, publisher, &req, taskType)
					delivery.Ack(false)
					continue
				}
			}

			// Persist the job. Payload is the request params; Type/Action come from
			// the flat function via ResolveFunction; the executor rebuilds a
			// TaskMessage from these.
			job := &jobstore.JobRecord{
				JobID:        generateJobID(),
				TaskID:       req.ID,
				TaskType:     string(taskType),
				Action:       action,
				VMIdentifier: vmIdentifier,
				Payload:      req.Params,
				Status:       jobstore.StatusQueued,
				CreatedAt:    time.Now().UTC(),
			}

			if err := store.Create(job); err != nil {
				logger.Error("failed to create job record", "task_id", req.ID, "error", err)
				delivery.Nack(false, false)
				continue
			}

			// ACK the RabbitMQ message - the job is now safely persisted
			delivery.Ack(false)

			logger.Info("job enqueued",
				"job_id", job.JobID,
				"task_id", job.TaskID,
				"function", req.Function,
				"type", job.TaskType,
				"action", job.Action,
				"vm_identifier", job.VMIdentifier)

			// Signal the dispatcher to evaluate queued jobs
			queueDispatcher.TriggerEvaluation()
		}
	}
}

// extractVMIdentifier pulls the VM identifier out of an AgentRequest's params,
// used for per-VM conflict detection. The backend sends {"vm_id": "..."}.
func extractVMIdentifier(taskType tasks.TaskType, params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	switch taskType {
	case tasks.TaskVMManagement, tasks.TaskVMEdit, tasks.TaskVMStatus:
		var p struct {
			VMID string `json:"vm_id"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.VMID != "" {
			return p.VMID
		}
	case tasks.TaskVMCreate:
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &p); err == nil {
			return p.Name
		}
	case tasks.TaskVMClone:
		var p struct {
			NewName string `json:"new_name"`
		}
		if err := json.Unmarshal(params, &p); err == nil {
			return p.NewName
		}
	}
	return ""
}

// isWriteTask returns true for task types subject to duplicate rejection.
func isWriteTask(taskType tasks.TaskType) bool {
	switch taskType {
	case tasks.TaskVMEdit, tasks.TaskVMCreate, tasks.TaskVMClone, tasks.TaskVMManagement:
		return true
	}
	return false
}

// publishConflictResponse publishes a "failed" AgentResponse for a rejected duplicate.
func publishConflictResponse(ctx context.Context, publisher *rabbitmq.Publisher, req *protocol.AgentRequest, taskType tasks.TaskType) {
	errMsg := "duplicate task rejected: a matching job is already queued or running"
	publisher.Publish(ctx, &tasks.TaskResponse{
		TaskID:     req.ID,
		Type:       taskType,
		Function:   req.Function,
		Status:     "failed",
		Error:      &errMsg,
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	})
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

// publishAgentStatus runs agent_status once and publishes the payload to the
// <hostid>.agent_status last-value queue.
func publishAgentStatus(ctx context.Context, d *tasks.Dispatcher, publisher *rabbitmq.Publisher, cfg *config.Config, logger *slog.Logger) {
	resp := d.Dispatch(ctx, &tasks.TaskMessage{
		TaskID: fmt.Sprintf("agent-status-%d", time.Now().Unix()),
		Type:   tasks.TaskAgentStatus,
	})
	kind, payload, ok := tasks.StatePayload(tasks.TaskAgentStatus, resp.Result)
	if !ok {
		logger.Warn("agent_status produced no payload")
		return
	}
	if err := publisher.PublishConfirmed(ctx, cfg.StateQueue(kind), payload); err != nil {
		logger.Warn("agent_status NOT confirmed by broker", "error", err)
	} else {
		logger.Info("published agent_status", "queue", cfg.StateQueue(kind))
	}
}

// publishUpgradeFailure reports a failed agent upgrade to the backend response queue.
// It runs in the short-lived "upgrade" process after upgrade.Run has failed and rolled
// back. The task_id comes from the pending response captured before the upgrade ran.
//
// It uses config.LoadForMessaging (no strict path validation) so a failure can still be
// reported even when the underlying cause is an invalid config path - the common case
// where the upgraded agent fails to start because of a bad template_path/local_iso_path.
func publishUpgradeFailure(logger *slog.Logger, pending *upgrade.PendingUpgradeResponse, upErr error) {
	if pending == nil || pending.TaskID == "" {
		logger.Warn("upgrade: no pending response available, cannot publish failure to backend")
		return
	}

	cfg, err := config.LoadForMessaging("")
	if err != nil {
		logger.Error("upgrade: cannot load messaging config to publish failure", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := rabbitmq.NewConnection(cfg.RabbitMQURL, logger)
	if err := conn.Connect(ctx); err != nil {
		logger.Error("upgrade: cannot connect to RabbitMQ to publish failure", "error", err)
		return
	}
	defer conn.Close()

	publisher := rabbitmq.NewPublisher(conn, cfg.ResponseQueue(), logger)
	if err := publisher.DeclareQueue(); err != nil {
		logger.Warn("upgrade: failed to declare response queue before publishing failure", "error", err)
	}

	errMsg := fmt.Sprintf("agent upgrade failed: %v", upErr)
	response := &tasks.TaskResponse{
		TaskID:   pending.TaskID,
		HostID:   cfg.HostID,
		Type:     tasks.TaskAgentUpgrade,
		Function: "host_update_agent",
		Status:   "failed",
		Result: map[string]interface{}{
			"version":     pending.Version,
			"upgraded":    false,
			"old_version": pending.OldVersion,
		},
		Error:      &errMsg,
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := publisher.Publish(ctx, response); err != nil {
		logger.Error("upgrade: failed to publish upgrade failure response", "task_id", pending.TaskID, "error", err)
		return
	}
	logger.Info("upgrade: published upgrade failure response to backend", "task_id", pending.TaskID)
}

// rotateLogFile checks if the log file exceeds 2 MB and truncates it if so.
// After creating (or truncating) the file, it attempts to set the NTFS
// compressed attribute so Windows stores it more efficiently.
func rotateLogFile(logFile string) {
	const maxLogSize = 2 << 20 // 2 MB

	info, err := os.Stat(logFile)
	if err != nil {
		return // file doesn't exist yet, nothing to rotate
	}

	if info.Size() <= maxLogSize {
		return // within limits
	}

	// Truncate the file
	os.Remove(logFile)

	// Recreate empty file and set NTFS compressed attribute
	f, err := os.Create(logFile)
	if err != nil {
		return
	}
	f.Close()

	setNTFSCompressed(logFile)
}

// setNTFSCompressed enables NTFS compression on the given file using
// DeviceIoControl with FSCTL_SET_COMPRESSION.
func setNTFSCompressed(path string) {
	const (
		FSCTL_SET_COMPRESSION      = 0x0009C040
		COMPRESSION_FORMAT_DEFAULT = 1
	)

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}

	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(handle)

	var bytesReturned uint32
	compressionFormat := uint16(COMPRESSION_FORMAT_DEFAULT)
	_ = syscall.DeviceIoControl(
		handle,
		FSCTL_SET_COMPRESSION,
		(*byte)(unsafe.Pointer(&compressionFormat)),
		2,
		nil,
		0,
		&bytesReturned,
		nil,
	)
}

func setupLogger(level, logFile string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	// Rotate log if > 2 MB
	rotateLogFile(logFile)

	// Try to open log file, fall back to stderr
	var handler slog.Handler
	file, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	} else {
		handler = slog.NewJSONHandler(file, &slog.HandlerOptions{Level: logLevel})
	}

	return slog.New(handler)
}
