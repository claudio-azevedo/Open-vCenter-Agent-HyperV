package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"ovc-agent/internal/config"
	"ovc-agent/internal/jobqueue"
	"ovc-agent/internal/jobstore"
	"ovc-agent/internal/rabbitmq"
	"ovc-agent/internal/tasks"
)

// RunJob is the entry point for worker processes invoked via `ovc-agent.exe job <job_id>`.
// It loads config, reads the job from the store, executes the task, publishes the result
// to RabbitMQ, and updates the job status in the database.
//
// The SQLite job store (WAL + busy_timeout) is shared with the service process; the
// worker opens its own handle briefly for each DB operation.
func RunJob(jobID string) {
	// 1. Load configuration
	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Set up logger using the same log file as the service
	logger := setupWorkerLogger(cfg.LogLevel, cfg.LogFile)
	logger.Info("worker started", "job_id", jobID, "pid", os.Getpid())

	// 2. Read the job record (open DB briefly, then close to release lock)
	job, err := readJob(jobID, logger)
	if err != nil {
		logger.Error("worker: failed to read job", "job_id", jobID, "error", err)
		fmt.Fprintf(os.Stderr, "worker: failed to read job: %v\n", err)
		os.Exit(1)
	}
	if job.Status != jobstore.StatusRunning {
		logger.Error("worker: job status is not running",
			"job_id", jobID, "status", job.Status)
		os.Exit(1)
	}

	// 3. Connect to RabbitMQ with 30s timeout for initial connection
	ctx := context.Background()
	connectCtx, connectCancel := context.WithTimeout(ctx, 30*time.Second)

	conn := rabbitmq.NewConnection(cfg.RabbitMQURL, logger)
	if err := conn.Connect(connectCtx); err != nil {
		connectCancel()
		logger.Error("worker: failed to connect to RabbitMQ",
			"job_id", jobID, "error", err)
		// Mark job as failed since we can't publish results
		updateJobStatus(jobID, jobstore.StatusRunning, jobstore.StatusFailed,
			"failed to connect to RabbitMQ: "+err.Error(), logger)
		os.Exit(1)
	}
	connectCancel()
	defer conn.Close()

	publisher := rabbitmq.NewPublisher(conn, cfg.ResponseQueue(), logger)

	// Declare the response queue (ensure it exists)
	if err := publisher.DeclareQueue(); err != nil {
		logger.Error("worker: failed to declare response queue",
			"job_id", jobID, "error", err)
		updateJobStatus(jobID, jobstore.StatusRunning, jobstore.StatusFailed,
			"failed to declare response queue: "+err.Error(), logger)
		os.Exit(1)
	}

	// 4. Execute the task
	response := executeTask(ctx, cfg, job, publisher, logger)

	// 5. Publish the result to the response queue - but NOT for the scheduler's
	// own periodic refresh jobs ("refresh-<kind>-<ts>", see jobqueue.Scheduler):
	// no backend Task is waiting on <hostid>.response for those, and the payload
	// goes to the dedicated state queue below. Emitting it just makes the backend
	// log an orphan response.
	if !jobqueue.IsPeriodicRefreshTaskID(job.TaskID) {
		if err := publisher.PublishConfirmed(ctx, cfg.ResponseQueue(), response); err != nil {
			logger.Error("worker: response NOT confirmed by RabbitMQ",
				"job_id", jobID, "task_id", response.TaskID, "error", err)
			updateJobStatus(jobID, jobstore.StatusRunning, jobstore.StatusFailed,
				"failed to publish response: "+err.Error(), logger)
			os.Exit(1)
		}
	}

	// Publish to applicable state queues based on task type
	publishToStateQueues(ctx, cfg, job, response, publisher, logger)

	// 6. Update job status in DB (open DB briefly).
	// Use TerminalStatus() rather than the wire Status: an agent-upgrade download
	// reports wire Status "in_progress" to the backend but carries InternalStatus
	// "completed", so the job must be marked completed internally to trigger drain
	// mode and launch the upgrade.
	now := time.Now().UTC()
	if response.TerminalStatus() == "completed" {
		err = updateJobStatusWithFields(jobID, jobstore.StatusRunning, jobstore.StatusCompleted, map[string]interface{}{
			"completed_at": now,
		}, logger)
	} else {
		errMsg := ""
		if response.Error != nil {
			errMsg = *response.Error
		}
		err = updateJobStatusWithFields(jobID, jobstore.StatusRunning, jobstore.StatusFailed, map[string]interface{}{
			"completed_at":  now,
			"error_message": errMsg,
		}, logger)
	}

	if err != nil {
		logger.Error("worker: failed to update job status",
			"job_id", jobID, "error", err)
	}

	// 7. Close RabbitMQ connection (deferred above) and exit
	logger.Info("worker finished",
		"job_id", jobID,
		"task_id", job.TaskID,
		"task_type", job.TaskType,
		"status", response.Status)

	if response.Status == "failed" {
		os.Exit(1)
	}
	os.Exit(0)
}

// readJob opens the job store briefly to read a single job record, then closes it
// immediately to release the file lock for other processes.
func readJob(jobID string, logger *slog.Logger) (*jobstore.JobRecord, error) {
	store, err := jobstore.OpenShared(config.JobStorePath())
	if err != nil {
		return nil, fmt.Errorf("open job store: %w", err)
	}
	defer store.Close()

	return store.GetByID(jobID)
}

// updateJobStatus opens the DB briefly to transition a job status with an error message.
func updateJobStatus(jobID, from, to, errMsg string, logger *slog.Logger) {
	store, err := jobstore.OpenShared(config.JobStorePath())
	if err != nil {
		logger.Error("worker: failed to open job store for status update",
			"job_id", jobID, "error", err)
		return
	}
	defer store.Close()

	now := time.Now().UTC()
	if updateErr := store.UpdateStatus(jobID, from, to, map[string]interface{}{
		"completed_at":  now,
		"error_message": errMsg,
	}); updateErr != nil {
		logger.Error("worker: failed to update job status",
			"job_id", jobID, "error", updateErr)
	}
}

// updateJobStatusWithFields opens the DB briefly to transition a job status with custom fields.
func updateJobStatusWithFields(jobID, from, to string, fields map[string]interface{}, logger *slog.Logger) error {
	store, err := jobstore.OpenShared(config.JobStorePath())
	if err != nil {
		return fmt.Errorf("open job store: %w", err)
	}
	defer store.Close()

	return store.UpdateStatus(jobID, from, to, fields)
}

// declareStateQueue declares the per-host queue a StatePayload kind publishes to.
// The *_metrics kinds are a short time-series and must be plain durable queues
// (no x-max-length); every other kind is a last-value queue.
func declareStateQueue(publisher *rabbitmq.Publisher, kind, queueName string) error {
	if strings.HasSuffix(kind, "_metrics") {
		return publisher.DeclarePlainQueue(queueName)
	}
	return publisher.DeclareStateQueue(queueName)
}

// publishToStateQueues publishes an inventory task's result to its last-value
// queue in the shape ovc-backend expects ({ reported_at, ... } - see
// tasks.StatePayload). Non-inventory tasks are a no-op.
func publishToStateQueues(ctx context.Context, cfg *config.Config, job *jobstore.JobRecord, response *tasks.TaskResponse, publisher *rabbitmq.Publisher, logger *slog.Logger) {
	if response.TerminalStatus() != "completed" {
		return
	}

	kind, payload, ok := tasks.StatePayload(tasks.TaskType(job.TaskType), response.Result)
	if !ok {
		return
	}

	stateQueue := cfg.StateQueue(kind)
	if err := declareStateQueue(publisher, kind, stateQueue); err != nil {
		logger.Warn("worker: failed to declare state queue", "queue", stateQueue, "error", err)
		return
	}

	if err := publisher.PublishConfirmed(ctx, stateQueue, payload); err != nil {
		logger.Error("worker: state queue publish NOT confirmed by broker",
			"job_id", job.JobID, "queue", stateQueue, "task_type", job.TaskType, "error", err)
	} else {
		logger.Info("worker: result confirmed on state queue",
			"job_id", job.JobID, "queue", stateQueue, "task_type", job.TaskType)
	}
}

// setupWorkerLogger creates a logger for the worker process, writing to the same log file
// as the service process. Falls back to stderr if the log file cannot be opened.
func setupWorkerLogger(level, logFile string) *slog.Logger {
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

	var handler slog.Handler
	file, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	} else {
		handler = slog.NewJSONHandler(file, &slog.HandlerOptions{Level: logLevel})
	}

	return slog.New(handler)
}
