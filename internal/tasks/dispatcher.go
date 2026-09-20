package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"ovc-agent/internal/hyperv"
)

// TaskType defines the type of task
type TaskType string

const (
	TaskAgentStatus       TaskType = "agent_status"
	TaskHardwareInventory TaskType = "hardware_inventory"
	TaskVMInventory       TaskType = "vm_inventory"
	TaskVMStatus          TaskType = "vm_status"
	TaskVMCreate          TaskType = "vm_create"
	TaskVMClone           TaskType = "vm_clone"
	TaskVMEdit            TaskType = "vm_edit"
	TaskVMManagement      TaskType = "vm_management"
	TaskHostManagement    TaskType = "host_management"
	TaskTemplateInventory TaskType = "template_inventory"
	TaskISOInventory      TaskType = "iso_inventory"
	TaskHostMetrics       TaskType = "host_metrics"
	TaskVMMetrics         TaskType = "vm_metrics"

	// TaskAgentUpgrade is the internal task type for the agent self-upgrade
	// (wire function "host_update_agent"). The string value is kept as
	// "artifact_download" for continuity with existing job records and the
	// upgrade code path.
	TaskAgentUpgrade     TaskType = "artifact_download"
	TaskArtifactDownload          = TaskAgentUpgrade
)

// TaskMessage is the internal representation of an inbound request, built by the
// consumer from a protocol.AgentRequest. Type/Action are resolved from the flat
// wire `function` via resolveFunction; Payload is the request `params`.
type TaskMessage struct {
	TaskID  string          `json:"task_id"`
	Type    TaskType        `json:"type"`
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}

// GetTaskID returns the task ID.
func (m *TaskMessage) GetTaskID() string { return m.TaskID }

// TaskResponse is the agent's internal response representation. It marshals to
// the backend's AgentResponse wire shape (see MarshalJSON) - the JSON tags on
// the fields below are for internal/debug use only and are NOT what goes on the
// wire.
type TaskResponse struct {
	TaskID   string      `json:"task_id"`
	HostID   string      `json:"host_id"`
	Type     TaskType    `json:"type"`
	Function string      `json:"function,omitempty"`
	Status   string      `json:"status"`
	Result   interface{} `json:"result"`
	VMStatus interface{} `json:"vm_status,omitempty"`
	// Template is the freshly exported template object (one TemplateInfo) attached
	// to a successful vm_export_template response so the backend can register the
	// row immediately instead of waiting for the next template_inventory.
	Template  interface{} `json:"template,omitempty"`
	Progress  *Progress   `json:"progress,omitempty"`
	Error     *string     `json:"error"`
	Timestamp string      `json:"timestamp"`

	// VMID/VMState let the backend update its fast VM-state cache immediately
	// (services/inventory.py::apply_response reads these on a terminal response).
	VMID       string `json:"vm_id,omitempty"`
	VMState    string `json:"vm_state,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	// InternalStatus decouples the internal lifecycle status from the status
	// reported to the backend (Status). It is never serialized to RabbitMQ.
	//
	// It exists for the agent-upgrade download phase: once the new binary is
	// downloaded, the wire Status is "in_progress" (the upgrade has NOT been
	// applied yet, so the backend must not see "completed"), while the internal
	// lifecycle must still be treated as "completed" so the worker marks the job
	// done and the service enters drain mode to launch the upgrade. The terminal
	// "completed" (upgraded: true) is published later by the new agent after it
	// restarts; a "failed" is published by the upgrade process on rollback.
	//
	// When empty, callers should fall back to Status (see TerminalStatus).
	InternalStatus string `json:"-"`
}

// TerminalStatus returns the status used to drive the internal job lifecycle
// (worker completion detection and job-store status). It prefers InternalStatus
// when set, otherwise falls back to the wire Status.
func (r *TaskResponse) TerminalStatus() string {
	if r.InternalStatus != "" {
		return r.InternalStatus
	}
	return r.Status
}

// MarshalJSON emits the backend's AgentResponse contract
// (ovc-backend/app/messaging/protocol.py): { id, function, status, progress,
// progress_message, result, error, vm_id, vm_state, vm_status, template,
// finished_at }. Internal-only fields (host_id, type, timestamp, InternalStatus)
// are dropped, the status vocabulary is normalized to running|succeeded|failed,
// and Progress splits into its percent (progress) and step text
// (progress_message). vm_status is the fresh full VM object the backend applies
// to the row immediately; template is the fresh template object after a
// vm_export_template.
func (r *TaskResponse) MarshalJSON() ([]byte, error) {
	fn := r.Function
	if fn == "" {
		fn = string(r.Type)
	}

	type wire struct {
		ID       string `json:"id"`
		Function string `json:"function"`
		Status   string `json:"status"`
		Progress *int   `json:"progress"`
		// ProgressMessage is the human-readable step the agent is on
		// ("Exporting VM (45%)", "Creating disk 2/3: …"). The backend keeps it on
		// the task row so the UI can show a live "Details" line.
		ProgressMessage string      `json:"progress_message,omitempty"`
		Result          interface{} `json:"result"`
		Error           *string     `json:"error"`
		VMID            *string     `json:"vm_id"`
		VMState         *string     `json:"vm_state"`
		// VMStatus is the fresh, full VM object after a VM-mutating task, so the
		// backend can update the row immediately (services/inventory.py
		// ::apply_response). It's a single normalized VMInfo.
		VMStatus interface{} `json:"vm_status,omitempty"`
		// Template is the freshly exported template (one TemplateInfo) after a
		// successful vm_export_template - the backend registers it right away.
		Template   interface{} `json:"template,omitempty"`
		FinishedAt *string     `json:"finished_at"`
	}

	w := wire{
		ID:       r.TaskID,
		Function: fn,
		Status:   normalizeStatus(r.Status),
		Result:   r.Result,
		Error:    r.Error,
		VMStatus: r.VMStatus,
		Template: r.Template,
	}
	if r.Progress != nil {
		p := r.Progress.Percent
		w.Progress = &p
		w.ProgressMessage = r.Progress.Message
	}
	if r.VMID != "" {
		w.VMID = &r.VMID
	}
	if r.VMState != "" {
		w.VMState = &r.VMState
	}
	if r.FinishedAt != "" {
		w.FinishedAt = &r.FinishedAt
	}
	return json.Marshal(w)
}

// normalizeStatus maps the agent's internal lifecycle vocabulary onto the three
// values the backend understands (running | succeeded | failed).
func normalizeStatus(s string) string {
	switch s {
	case "completed", "succeeded", "success", "ok":
		return "succeeded"
	case "failed", "error":
		return "failed"
	case "in_progress", "running", "pending", "queued", "":
		return "running"
	default:
		return s
	}
}

// Progress holds progress information for long-running tasks
type Progress struct {
	Percent int    `json:"percent"`
	Message string `json:"message"`
}

// ProgressPublisher is a callback to publish progress updates during long-running tasks
type ProgressPublisher func(ctx context.Context, response *TaskResponse)

// PostActionPublisher is a callback to publish additional messages after state-changing tasks
type PostActionPublisher func(ctx context.Context, response *TaskResponse)

// ClusterInfo holds cached cluster membership information detected at startup
type ClusterInfo struct {
	IsClusterNode bool     // true if this node is part of a Microsoft Failover Cluster
	ClusterState  string   // e.g. "Up", "Down", "Not Clustered"
	ClusterNodes  []string // all node names in the cluster (empty if standalone)
}

// Dispatcher routes tasks to the appropriate handler
type Dispatcher struct {
	hostID               string
	taskTimeout          time.Duration
	startTime            time.Time
	logger               *slog.Logger
	templatePath         string
	localISOPath         string
	additionalStorage    []string
	vmRefreshInterval    int
	hostRefreshInterval  int
	cluster              ClusterInfo
	onVMStateChange      PostActionPublisher
	onProgress           ProgressPublisher
	onHardwareRefresh    PostActionPublisher
	onVMInventoryRefresh PostActionPublisher
}

// NewDispatcher creates a new task dispatcher
func NewDispatcher(hostID string, taskTimeout time.Duration, templatePath string, localISOPath string, additionalStorage []string, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		hostID:            hostID,
		taskTimeout:       taskTimeout,
		templatePath:      templatePath,
		localISOPath:      localISOPath,
		additionalStorage: additionalStorage,
		startTime:         time.Now(),
		logger:            logger,
	}
}

// SetRefreshIntervals records the configured periodic-refresh intervals so they
// can be reported in agent_status.
func (d *Dispatcher) SetRefreshIntervals(vmSeconds, hostSeconds int) {
	d.vmRefreshInterval = vmSeconds
	d.hostRefreshInterval = hostSeconds
}

// GetHostID returns the host ID configured for this dispatcher.
func (d *Dispatcher) GetHostID() string {
	return d.hostID
}

// SetOnVMStateChange registers a callback that fires after VM state changes (start/stop)
func (d *Dispatcher) SetOnVMStateChange(fn PostActionPublisher) {
	d.onVMStateChange = fn
}

// DetectCluster checks at startup whether this node is part of a Microsoft Failover Cluster.
// The result is cached for the lifetime of the agent, avoiding repeated PowerShell calls
// on every hardware_inventory and vm_inventory execution.
func (d *Dispatcher) DetectCluster(ctx context.Context) {
	script := `
$result = @{ isClusterNode = $false; clusterState = "Not Clustered"; clusterNodes = @() }
if (Get-Command Get-ClusterNode -ErrorAction SilentlyContinue) {
    try {
        $nodes = Get-ClusterNode -ErrorAction Stop
        $localNode = $nodes | Where-Object { $_.Name -eq $env:COMPUTERNAME }
        if ($localNode) {
            $result.isClusterNode = $true
            $result.clusterState = $localNode.State.ToString()
            $result.clusterNodes = @($nodes | ForEach-Object { $_.Name })
        }
    } catch {}
}
$result | ConvertTo-Json -Compress
`
	output, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		d.logger.Warn("cluster detection failed, assuming standalone", "error", err)
		d.cluster = ClusterInfo{
			IsClusterNode: false,
			ClusterState:  "Not Clustered",
			ClusterNodes:  []string{},
		}
		return
	}

	output = trimPowerShellOutput(output)

	var parsed struct {
		IsClusterNode bool     `json:"isClusterNode"`
		ClusterState  string   `json:"clusterState"`
		ClusterNodes  []string `json:"clusterNodes"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		d.logger.Warn("failed to parse cluster detection output, assuming standalone", "error", err)
		d.cluster = ClusterInfo{
			IsClusterNode: false,
			ClusterState:  "Not Clustered",
			ClusterNodes:  []string{},
		}
		return
	}

	d.cluster = ClusterInfo{
		IsClusterNode: parsed.IsClusterNode,
		ClusterState:  parsed.ClusterState,
		ClusterNodes:  parsed.ClusterNodes,
	}

	if d.cluster.IsClusterNode {
		d.logger.Info("cluster detected", "state", d.cluster.ClusterState, "nodes", d.cluster.ClusterNodes)
	} else {
		d.logger.Info("standalone host (no cluster membership)")
	}
}

// SetOnProgress registers a callback for publishing progress updates on long-running tasks
func (d *Dispatcher) SetOnProgress(fn ProgressPublisher) {
	d.onProgress = fn
}

// SetOnHardwareRefresh registers a callback for publishing hardware inventory to state queue
func (d *Dispatcher) SetOnHardwareRefresh(fn PostActionPublisher) {
	d.onHardwareRefresh = fn
}

// SetOnVMInventoryRefresh registers a callback for publishing VM inventory to state queue
func (d *Dispatcher) SetOnVMInventoryRefresh(fn PostActionPublisher) {
	d.onVMInventoryRefresh = fn
}

// reportProgress publishes a progress update for a task in progress
func (d *Dispatcher) reportProgress(ctx context.Context, taskID string, taskType TaskType, percent int, message string) {
	if d.onProgress == nil {
		return
	}
	d.onProgress(ctx, &TaskResponse{
		TaskID:    taskID,
		HostID:    d.hostID,
		Type:      taskType,
		Status:    "in_progress",
		Progress:  &Progress{Percent: percent, Message: message},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// isVMStateChangingTask returns true for tasks that modify VM state
func isVMStateChangingTask(t TaskType) bool {
	switch t {
	case TaskVMCreate, TaskVMClone, TaskVMEdit, TaskVMManagement:
		return true
	}
	return false
}

// withAction merges an "action" key into a params object so the existing
// vm_management / host_management handlers (which read payload.action) keep
// working with the backend's flat `function` model.
func withAction(payload json.RawMessage, action string) json.RawMessage {
	if action == "" {
		return payload
	}
	m := map[string]json.RawMessage{}
	if len(payload) > 0 && string(payload) != "null" {
		_ = json.Unmarshal(payload, &m)
	}
	b, _ := json.Marshal(action)
	m["action"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// Dispatch handles an incoming task message and returns the response
func (d *Dispatcher) Dispatch(ctx context.Context, msg *TaskMessage) *TaskResponse {
	// The backend sends a flat `function` + `params`; resolveFunction has already
	// split that into msg.Type + msg.Action. Fold the action back into the params
	// object for the handlers that dispatch on payload.action.
	if msg.Action != "" {
		msg.Payload = withAction(msg.Payload, msg.Action)
	}

	taskTimeout := d.taskTimeout
	if msg.Type == TaskVMManagement {
		switch msg.Action {
		case "vm_batch_start", "vm_batch_stop", "vm_move", "vm_rename", "vm_export_template":
			// These actions run long PowerShell operations (storage migration, export, batch).
			// Extend taskCtx so reportProgress and post-completion steps don't use a cancelled context.
			if taskTimeout < 30*time.Minute {
				taskTimeout = 30 * time.Minute
			}
		case "vm_shutdown":
			// A graceful guest-OS shutdown can run for minutes; keep taskCtx a
			// touch under the jobqueue.Monitor process-kill (600s) so the handler
			// can report a timeout instead of being killed mid-flight.
			if taskTimeout < 9*time.Minute {
				taskTimeout = 9 * time.Minute
			}
		}
	}

	// vm_create / vm_edit can create or expand multiple disks (Fixed VHDs
	// zero-fill at ~10 min each) - extend the task context accordingly.
	if (msg.Type == TaskVMEdit || msg.Type == TaskVMCreate) && taskTimeout < 30*time.Minute {
		taskTimeout = 30 * time.Minute
	}

	taskCtx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()

	d.logger.Info("dispatching task", "task_id", msg.GetTaskID(), "type", msg.Type)

	var result interface{}
	var taskErr error

	// Recover from panics inside task handlers to ensure failure is always reported
	func() {
		defer func() {
			if r := recover(); r != nil {
				taskErr = fmt.Errorf("internal panic: %v", r)
				d.logger.Error("task handler panicked", "task_id", msg.GetTaskID(), "type", msg.Type, "panic", r)
			}
		}()

		switch msg.Type {
		case TaskAgentStatus:
			result, taskErr = d.handleAgentStatus(taskCtx)
		case TaskHardwareInventory:
			result, taskErr = d.handleHardwareInventory(taskCtx)
		case TaskVMInventory:
			// VM inventory runs multiple PowerShell blocks sequentially. On hosts with 50+ VMs,
			// this can exceed the default task_timeout. Use a dedicated 10-minute context.
			invCtx, invCancel := context.WithTimeout(ctx, 10*time.Minute)
			result, taskErr = d.handleVMInventory(invCtx)
			invCancel()
		case TaskVMStatus:
			result, taskErr = d.handleVMStatus(taskCtx, msg.Payload)
		case TaskVMCreate:
			result, taskErr = d.handleVMCreate(taskCtx, msg.Payload)
		case TaskVMClone:
			result, taskErr = d.handleVMClone(taskCtx, msg.GetTaskID(), msg.Payload)
		case TaskVMEdit:
			result, taskErr = d.handleVMEdit(taskCtx, msg.GetTaskID(), msg.Payload)
		case TaskVMManagement:
			result, taskErr = d.handleVMManagement(taskCtx, msg.GetTaskID(), msg.Payload)
		case TaskHostManagement:
			result, taskErr = d.handleHostManagement(taskCtx, msg.Payload)
		case TaskTemplateInventory:
			result, taskErr = d.handleTemplateInventory(taskCtx)
		case TaskISOInventory:
			result, taskErr = d.handleISOInventory(taskCtx)
		case TaskHostMetrics:
			result, taskErr = d.handleHostMetrics(taskCtx)
		case TaskVMMetrics:
			result, taskErr = d.handleVMMetrics(taskCtx)
		case TaskAgentUpgrade:
			// The agent self-upgrade runs asynchronously in a goroutine so it
			// doesn't block task consumption. It publishes its own progress and
			// terminal responses via d.onProgress.
			go d.handleAgentUpgrade(ctx, msg.GetTaskID(), msg.Payload)

			// Return an immediate "in_progress" ACK response
			result = nil
			taskErr = nil
		default:
			taskErr = fmt.Errorf("unknown task type: %s", msg.Type)
		}
	}()

	fn := wireFunction(msg.Type, msg.Action)

	// The agent upgrade is async - return immediately with in_progress.
	if msg.Type == TaskAgentUpgrade && taskErr == nil {
		return &TaskResponse{
			TaskID:   msg.GetTaskID(),
			HostID:   d.hostID,
			Type:     msg.Type,
			Function: fn,
			Status:   "in_progress",
			Progress: &Progress{Percent: 0, Message: "Task accepted, starting agent upgrade..."},
		}
	}

	response := &TaskResponse{
		TaskID:     msg.GetTaskID(),
		HostID:     d.hostID,
		Type:       msg.Type,
		Function:   fn,
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if taskErr != nil {
		response.Status = "failed"
		errStr := taskErr.Error()
		response.Error = &errStr
		d.logger.Error("task failed", "task_id", msg.GetTaskID(), "type", msg.Type, "error", taskErr)
	} else {
		response.Status = "completed"
		response.Result = result
		d.logger.Info("task completed", "task_id", msg.GetTaskID(), "type", msg.Type)

		// vm_export_template: hand the fresh template object back so the backend
		// registers the row now instead of waiting for the next template_inventory.
		if r, ok := result.(*VMExportTemplateResult); ok && r != nil && r.Template != nil {
			response.Template = r.Template
		}

		// After successful VM state change, fetch fresh vm_status and attach to response
		if isVMStateChangingTask(msg.Type) {
			// Skip vm_status fetch for vm_delete - the VM no longer exists
			// Skip for refresh_status - the result IS the vm_status
			// Skip for notes_edit - the result IS the vm_status
			if !isDeleteAction(msg.Type, msg.Payload) && !isRefreshStatusAction(msg.Type, msg.Payload) && !isNotesEditAction(msg.Type, msg.Payload) {
				vmIdentifier := extractVMIdentifier(msg.Type, msg.Payload, result)
				if vmIdentifier != "" {
					vmStatus, err := d.handleVMStatus(ctx, json.RawMessage(vmIdentifier))
					if err != nil {
						d.logger.Warn("failed to fetch vm_status after state change", "error", err)
					} else if vmStatus != nil && len(vmStatus.VMs) > 0 {
						response.VMStatus = vmStatus.VMs[0]
						response.VMID = vmStatus.VMs[0].ID
						response.VMState = vmStatus.VMs[0].State
					}
				}
			}

			// For refresh_status or notes_edit, attach the result directly as vm_status
			if isRefreshStatusAction(msg.Type, msg.Payload) || isNotesEditAction(msg.Type, msg.Payload) {
				if vmResult, ok := result.(*VMInventoryResult); ok && vmResult != nil && len(vmResult.VMs) > 0 {
					response.VMStatus = vmResult.VMs[0]
					response.VMID = vmResult.VMs[0].ID
					response.VMState = vmResult.VMs[0].State
				}
			}

			// Trigger inventory refresh to state queue (skip for refresh_status - it's read-only)
			if d.onVMStateChange != nil && !isRefreshStatusAction(msg.Type, msg.Payload) {
				go func() {
					inventory, err := d.handleVMInventory(ctx)
					if err != nil {
						d.logger.Warn("failed to refresh VM inventory after state change", "error", err)
						return
					}
					refreshResponse := &TaskResponse{
						TaskID:    fmt.Sprintf("refresh-vm-after-%s-%d", msg.Type, time.Now().Unix()),
						HostID:    d.hostID,
						Type:      TaskVMInventory,
						Status:    "completed",
						Result:    inventory,
						Timestamp: time.Now().UTC().Format(time.RFC3339),
					}
					d.onVMStateChange(ctx, refreshResponse)
				}()
			}
		}
	}

	return response
}

// isDeleteAction returns true if this is a vm_management task with action "vm_delete"
func isDeleteAction(taskType TaskType, payload json.RawMessage) bool {
	if taskType != TaskVMManagement {
		return false
	}
	var p struct {
		Action string `json:"action"`
	}
	if payload != nil {
		json.Unmarshal(payload, &p)
	}
	return p.Action == "vm_delete"
}

// isRefreshStatusAction returns true if this is a vm_management task with action "refresh_status"
func isRefreshStatusAction(taskType TaskType, payload json.RawMessage) bool {
	if taskType != TaskVMManagement {
		return false
	}
	var p struct {
		Action string `json:"action"`
	}
	if payload != nil {
		json.Unmarshal(payload, &p)
	}
	return p.Action == "refresh_status"
}

// isNotesEditAction returns true if this is a vm_management task with action "notes_edit"
func isNotesEditAction(taskType TaskType, payload json.RawMessage) bool {
	if taskType != TaskVMManagement {
		return false
	}
	var p struct {
		Action string `json:"action"`
	}
	if payload != nil {
		json.Unmarshal(payload, &p)
	}
	return p.Action == "notes_edit"
}

// extractVMIdentifier builds a JSON payload for handleVMStatus based on the task type.
// It tries to get vm_id or vmName from the original payload or from the result.
func extractVMIdentifier(taskType TaskType, payload json.RawMessage, result interface{}) string {
	// For vm_management and vm_edit, payload has vm_id
	if taskType == TaskVMManagement || taskType == TaskVMEdit {
		var p struct {
			VMID   string `json:"vm_id"`
			VMName string `json:"vm_name"`
		}
		if payload != nil {
			json.Unmarshal(payload, &p)
		}
		if p.VMID != "" {
			b, _ := json.Marshal(map[string]string{"vm_id": p.VMID})
			return string(b)
		}
		if p.VMName != "" {
			b, _ := json.Marshal(map[string]string{"vmName": p.VMName})
			return string(b)
		}
	}

	// For vm_create or vm_clone, result has vmName
	if taskType == TaskVMCreate || taskType == TaskVMClone {
		if r, ok := result.(*VMCreateResult); ok && r != nil && r.VMName != "" {
			b, _ := json.Marshal(map[string]string{"vmName": r.VMName})
			return string(b)
		}
	}

	return ""
}
