package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"ovc-agent/internal/config"
	"ovc-agent/internal/jobstore"
	"ovc-agent/internal/rabbitmq"
	"ovc-agent/internal/tasks"
)

// executeTask runs the appropriate task handler for the given job and returns the response.
// It creates a tasks.Dispatcher configured for the worker process and delegates execution.
// For most task types, it calls dispatcher.Dispatch which handles routing internally.
// For artifact_download, the worker runs it synchronously (unlike the service process which
// fires it asynchronously) since the worker IS the async execution context.
func executeTask(ctx context.Context, cfg *config.Config, job *jobstore.JobRecord, publisher *rabbitmq.Publisher, logger *slog.Logger) *tasks.TaskResponse {
	taskTimeout := time.Duration(cfg.TaskTimeout) * time.Second
	dispatcher := tasks.NewDispatcher(
		cfg.HostID,
		taskTimeout,
		cfg.TemplatePath,
		cfg.LocalISOPath,
		cfg.AdditionalVMStorage,
		logger,
	)
	dispatcher.SetRefreshIntervals(cfg.RefreshIntervalVMs, cfg.RefreshIntervalHost)

	// publishState converts an inventory refresh TaskResponse into the backend's
	// last-value queue payload and publishes it.
	publishState := func(ctx context.Context, resp *tasks.TaskResponse) {
		kind, payload, ok := tasks.StatePayload(resp.Type, resp.Result)
		if !ok {
			return
		}
		stateQueue := cfg.StateQueue(kind)
		if err := declareStateQueue(publisher, kind, stateQueue); err != nil {
			logger.Warn("failed to declare state queue", "queue", stateQueue, "error", err)
		}
		if err := publisher.PublishConfirmed(ctx, stateQueue, payload); err != nil {
			logger.Error("inventory refresh publish NOT confirmed by broker", "queue", stateQueue, "error", err)
		} else {
			logger.Info("inventory refresh confirmed on state queue", "queue", stateQueue)
		}
	}

	// Progress + terminal responses go to the response queue (AgentResponse shape).
	dispatcher.SetOnProgress(func(ctx context.Context, resp *tasks.TaskResponse) {
		if err := publisher.Publish(ctx, resp); err != nil {
			logger.Warn("failed to publish progress update",
				"task_id", resp.TaskID, "type", resp.Type, "error", err)
		}
	})

	// After VM-mutating / host_management refresh tasks, push fresh inventory to
	// the appropriate last-value queue.
	dispatcher.SetOnVMStateChange(publishState)
	dispatcher.SetOnHardwareRefresh(publishState)
	dispatcher.SetOnVMInventoryRefresh(publishState)

	// Detect cluster membership (needed for HA operations and cluster-aware inventory)
	dispatcher.DetectCluster(ctx)

	// Build the TaskMessage from the job record
	msg := &tasks.TaskMessage{
		TaskID:  job.TaskID,
		Type:    tasks.TaskType(job.TaskType),
		Action:  job.Action,
		Payload: json.RawMessage(job.Payload),
	}

	// For artifact_download, the service's Dispatch() returns immediately with "in_progress"
	// and runs the download in a goroutine. In the worker process, we want synchronous
	// execution - the worker IS the async context. We handle this by calling Dispatch()
	// which will:
	//   1. Fire the goroutine for artifact_download
	//   2. Return an "in_progress" response immediately
	// We then need to wait for the goroutine to finish and get the final result.
	//
	// However, since the dispatcher's handleArtifactDownload publishes its own terminal
	// response (completed/failed) via onProgress, the worker can simply:
	//   - Let Dispatch() fire the goroutine
	//   - Wait for the process to complete (the goroutine will run and publish results)
	//   - Return a synthetic response based on what happened
	//
	// Actually, the cleanest approach: for artifact_download in the worker context,
	// we let Dispatch() handle it. The goroutine publishes progress and terminal responses.
	// The worker waits for the goroutine to complete by using a done channel via onProgress.
	// When the final "completed" or "failed" response is published via onProgress, we capture it.
	if tasks.TaskType(job.TaskType) == tasks.TaskArtifactDownload {
		return executeArtifactDownload(ctx, dispatcher, msg, publisher, logger)
	}

	// For all other task types, Dispatch() runs synchronously and returns the final response
	return dispatcher.Dispatch(ctx, msg)
}

// executeArtifactDownload handles the artifact_download task synchronously in the worker.
// The service dispatcher fires artifact_download in a goroutine and returns "in_progress",
// but in the worker we need to wait for completion and capture the final response.
func executeArtifactDownload(ctx context.Context, dispatcher *tasks.Dispatcher, msg *tasks.TaskMessage, publisher *rabbitmq.Publisher, logger *slog.Logger) *tasks.TaskResponse {
	// Channel to capture the terminal response (completed or failed)
	done := make(chan *tasks.TaskResponse, 1)

	// Override the onProgress callback to intercept the terminal response.
	// artifact_download publishes its own completed/failed via onProgress.
	dispatcher.SetOnProgress(func(ctx context.Context, resp *tasks.TaskResponse) {
		// Publish all messages (progress and terminal) to the response queue
		if err := publisher.Publish(ctx, resp); err != nil {
			logger.Warn("failed to publish artifact_download update",
				"task_id", resp.TaskID, "status", resp.Status, "error", err)
		}

		// Capture terminal responses (completed or failed) to signal we're done.
		// Use TerminalStatus() rather than the wire Status: an agent-upgrade download
		// publishes wire Status "in_progress" but carries InternalStatus "completed"
		// so the worker still treats the download job as done (and drain mode starts).
		if ts := resp.TerminalStatus(); ts == "completed" || ts == "failed" {
			select {
			case done <- resp:
			default:
			}
		}
	})

	// Dispatch fires the goroutine and returns "in_progress" immediately
	dispatcher.Dispatch(ctx, msg)

	// Wait for the goroutine to publish a terminal response
	// Use a generous timeout - artifact downloads can take up to 1 hour
	timeout := time.After(1 * time.Hour)
	select {
	case resp := <-done:
		return resp
	case <-timeout:
		errMsg := fmt.Sprintf("artifact_download timed out after 1 hour")
		logger.Error("artifact_download: worker timeout", "task_id", msg.GetTaskID())
		return &tasks.TaskResponse{
			TaskID:    msg.GetTaskID(),
			HostID:    dispatcher.GetHostID(),
			Type:      tasks.TaskArtifactDownload,
			Status:    "failed",
			Error:     &errMsg,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
	case <-ctx.Done():
		errMsg := fmt.Sprintf("artifact_download cancelled: %v", ctx.Err())
		logger.Error("artifact_download: context cancelled", "task_id", msg.GetTaskID(), "error", ctx.Err())
		return &tasks.TaskResponse{
			TaskID:    msg.GetTaskID(),
			HostID:    dispatcher.GetHostID(),
			Type:      tasks.TaskArtifactDownload,
			Status:    "failed",
			Error:     &errMsg,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}
	}
}
