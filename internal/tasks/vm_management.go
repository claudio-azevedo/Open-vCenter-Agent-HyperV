package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ovc-agent/internal/hyperv"
)

// VMManagementPayload is the generic payload for vm_management tasks.
//
// The wire contract (ovc-backend docs/agent-queue-contract.md) is a single flat
// params object: vm_id, action, and every action-specific key sit side by side.
// Raw holds that whole object so each per-action handler can unmarshal the keys
// it needs into its own typed struct.
type VMManagementPayload struct {
	VMID   string `json:"vm_id"`
	VMName string `json:"vm_name"`
	Action string `json:"action"`

	// Raw is the full params object, set by handleVMManagement.
	Raw json.RawMessage `json:"-"`
}

// params decodes the flat params object into a typed per-action struct. The
// object also carries vm_id/vm_name/action; unknown keys are ignored. Callers
// then validate the fields they require.
func (p VMManagementPayload) params(dst any) error {
	if len(p.Raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(p.Raw, dst); err != nil {
		return fmt.Errorf("invalid params for %s: %v", p.Action, err)
	}
	return nil
}

// VMManagementResult holds the generic result for vm_management tasks
type VMManagementResult struct {
	VMID   string `json:"vmId"`
	VMName string `json:"vmName,omitempty"`
	Action string `json:"action"`
	Status string `json:"status"`
	// Set by vm_delete when remove_files was requested: whether the VM folder was
	// deleted from disk, and a note when it could not be.
	FilesRemoved *bool  `json:"filesRemoved,omitempty"`
	Warning      string `json:"warning,omitempty"`
}

// VMBatchTarget identifies one VM in a start/stop batch.
type VMBatchTarget struct {
	VMID   string `json:"vm_id"`
	VMName string `json:"vm_name"`
}

// VMBatchParams holds the ordered VM list for a batch operation.
type VMBatchParams struct {
	VMs []VMBatchTarget `json:"vms"`
}

// VMBatchItemResult reports the outcome of one VM operation.
type VMBatchItemResult struct {
	VMID          string `json:"vmId"`
	VMName        string `json:"vmName"`
	Action        string `json:"action"`
	Status        string `json:"status"`
	PreviousState string `json:"previousState,omitempty"`
	State         string `json:"state,omitempty"`
	Changed       bool   `json:"changed"`
	Error         string `json:"error,omitempty"`
}

// VMBatchResult summarizes all per-VM results in input order.
type VMBatchResult struct {
	Action    string              `json:"action"`
	Status    string              `json:"status"`
	Total     int                 `json:"total"`
	Succeeded int                 `json:"succeeded"`
	Failed    int                 `json:"failed"`
	VMs       []VMBatchItemResult `json:"vms"`
}

// vmBatchStateOp is the parsed result of the combined per-VM batch script that
// reads current state, verifies the VM name matches, and performs the start/stop
// operation in a single PowerShell execution. PreviousState/State mirror the
// state read before the operation, Changed reports whether the op ran, OpError
// carries a controlled operation-failure message, and Mismatch flags a VM-name
// mismatch (so Go reproduces the exact mismatch error without a second spawn).
type vmBatchStateOp struct {
	Name          string `json:"name"`
	PreviousState string `json:"previousState"`
	State         string `json:"state"`
	Changed       bool   `json:"changed"`
	OpError       string `json:"opError"`
	Mismatch      bool   `json:"mismatch"`
}

const vmBatchOperationDelay = 10 * time.Second

// VMDeleteParams holds the params for the vm_delete action.
type VMDeleteParams struct {
	RemoveFiles bool `json:"remove_files"`
}

// extractPSErrorDetail extracts a human-readable error message from a PowerShell error.
// It strips known markers (e.g. "ENABLE_HA_FAILED:") and the "powershell error:" prefix,
// returning just the meaningful exception message for the task response.
func extractPSErrorDetail(err error, markers ...string) string {
	msg := err.Error()

	// Strip "powershell error: " or "powershell execution failed: " prefix
	msg = strings.TrimPrefix(msg, "powershell error: ")
	msg = strings.TrimPrefix(msg, "powershell execution failed: ")

	// Strip known marker prefixes (e.g. "ENABLE_HA_FAILED: ")
	for _, marker := range markers {
		if idx := strings.Index(msg, marker); idx >= 0 {
			msg = strings.TrimSpace(msg[idx+len(marker):])
			// Remove leading ": " if present
			msg = strings.TrimPrefix(msg, ": ")
			msg = strings.TrimPrefix(msg, " ")
			break
		}
	}

	// Final cleanup
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "unknown error (no details from PowerShell)"
	}
	return msg
}

func (d *Dispatcher) handleVMManagement(ctx context.Context, taskID string, payload json.RawMessage) (interface{}, error) {
	if payload == nil || string(payload) == "null" || string(payload) == "{}" {
		return nil, fmt.Errorf("payload is required for vm_management")
	}

	var p VMManagementPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("invalid payload: %v", err)
	}
	// Every per-action handler reads its keys straight off the flat params object.
	p.Raw = payload

	if p.Action == "" {
		return nil, fmt.Errorf("action is required for vm_management")
	}
	if p.Action == "vm_batch_start" || p.Action == "vm_batch_stop" {
		return d.handleVMBatchAction(ctx, taskID, p)
	}
	if p.VMID == "" {
		return nil, fmt.Errorf("vm_id is required for vm_management")
	}

	// The backend addresses VMs by vm_id; most handlers below work with the VM
	// name, so resolve it once here when it wasn't supplied.
	if p.VMName == "" {
		if name, err := d.resolveVMName(ctx, p.VMID); err == nil {
			p.VMName = name
		}
	}

	switch p.Action {
	case "vm_start":
		return d.handleVMStartAction(ctx, p)
	case "vm_stop":
		return d.handleVMStopAction(ctx, p)
	case "vm_shutdown":
		return d.handleVMShutdownAction(ctx, p)
	case "vm_restart":
		return d.handleVMRestartAction(ctx, p)
	case "vm_pause":
		return d.handleVMPauseAction(ctx, p)
	case "vm_resume":
		return d.handleVMResumeAction(ctx, p)
	case "vm_delete":
		return d.handleVMDelete(ctx, p)
	case "eject_dvd":
		return d.handleEjectDVD(ctx, p)
	case "mount_dvd":
		return d.handleMountDVD(ctx, p)
	case "enable_ha":
		return d.handleEnableHA(ctx, p)
	case "disable_ha":
		return d.handleDisableHA(ctx, p)
	case "vm_migrate":
		return d.handleVMMigrate(ctx, p)
	case "snapshot_create":
		return d.handleSnapshotCreate(ctx, p)
	case "snapshot_remove":
		return d.handleSnapshotRemove(ctx, p)
	case "snapshot_restore":
		return d.handleSnapshotRestore(ctx, p)
	case "refresh_status":
		return d.handleRefreshStatus(ctx, p)
	case "notes_edit":
		return d.handleNotesEdit(ctx, p)
	case "vm_export_template":
		return d.handleVMExportTemplate(ctx, taskID, p)
	case "vm_rename":
		return d.handleVMRename(ctx, taskID, p)
	case "vm_move":
		return d.handleVMMove(ctx, taskID, p)
	case "vm_startup_change":
		return d.handleVMStartupChange(ctx, p)
	case "vm_enable_metrics":
		return d.handleVMResourceMetering(ctx, p, true)
	case "vm_disable_metrics":
		return d.handleVMResourceMetering(ctx, p, false)
	default:
		return nil, fmt.Errorf("unknown vm_management action: %s", p.Action)
	}
}

func (d *Dispatcher) handleVMBatchAction(ctx context.Context, taskID string, p VMManagementPayload) (*VMBatchResult, error) {
	var params VMBatchParams
	if len(p.Raw) == 0 || string(p.Raw) == "null" {
		return nil, fmt.Errorf("vms is required for %s", p.Action)
	}
	if err := json.Unmarshal(p.Raw, &params); err != nil {
		return nil, fmt.Errorf("invalid params for %s: %v", p.Action, err)
	}
	if len(params.VMs) == 0 {
		return nil, fmt.Errorf("vms must contain at least one VM for %s", p.Action)
	}

	seen := make(map[string]struct{}, len(params.VMs))
	for i := range params.VMs {
		params.VMs[i].VMID = strings.TrimSpace(params.VMs[i].VMID)
		params.VMs[i].VMName = strings.TrimSpace(params.VMs[i].VMName)
		if params.VMs[i].VMID == "" {
			return nil, fmt.Errorf("vms[%d].vm_id is required", i)
		}
		if !isCanonicalGUID(params.VMs[i].VMID) {
			return nil, fmt.Errorf("vms[%d].vm_id must be a canonical GUID", i)
		}
		if params.VMs[i].VMName == "" {
			return nil, fmt.Errorf("vms[%d].vm_name is required", i)
		}
		key := strings.ToLower(params.VMs[i].VMID)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("vms[%d].vm_id is duplicated: %s", i, params.VMs[i].VMID)
		}
		seen[key] = struct{}{}
	}

	result := &VMBatchResult{
		Action: p.Action,
		Total:  len(params.VMs),
		VMs:    make([]VMBatchItemResult, 0, len(params.VMs)),
	}
	d.reportProgress(ctx, taskID, TaskVMManagement, 0,
		fmt.Sprintf("Starting %s for %d VM(s)", p.Action, result.Total))

	for i, target := range params.VMs {
		item, vmStatus := d.processVMBatchVM(ctx, p.Action, target)
		result.VMs = append(result.VMs, item)
		if item.Status == "ok" {
			result.Succeeded++
		} else {
			result.Failed++
		}

		percent := (i + 1) * 100 / result.Total
		message := fmt.Sprintf("VM %s processed successfully (%d/%d)", item.VMName, i+1, result.Total)
		if item.Error != "" {
			message = fmt.Sprintf("VM %s failed: %s (%d/%d)", item.VMName, item.Error, i+1, result.Total)
		} else if !item.Changed {
			message = fmt.Sprintf("VM %s was already %s (%d/%d)", item.VMName, item.State, i+1, result.Total)
		}
		d.publishVMBatchProgress(ctx, taskID, percent, message, item, vmStatus)

		if i < len(params.VMs)-1 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%s interrupted after %d of %d VMs: %v", p.Action, i+1, result.Total, ctx.Err())
			case <-time.After(vmBatchOperationDelay):
			}
		}
	}

	result.Status = deriveVMBatchStatus(result.Succeeded, result.Failed)

	return result, nil
}

// deriveVMBatchStatus derives the aggregate batch status from the per-item
// success/failure counts, preserving the original precedence: no failures =>
// "completed", no successes => "failed", otherwise "partial". Extracted verbatim
// from handleVMBatchAction as a pure helper for preservation testing.
func deriveVMBatchStatus(succeeded, failed int) string {
	switch {
	case failed == 0:
		return "completed"
	case succeeded == 0:
		return "failed"
	default:
		return "partial"
	}
}

// mapVMMoveProgress maps a Move-VMStorage job completion percentage (0-100) onto the
// vm_move task progress band 5-95. Extracted verbatim from the vm_move progress
// closure so the exact mapping (5 + jobPercent*90/100) is regression-testable without
// a live storage migration; the closure now calls this pure helper. The extraction is
// behavior-preserving and adds no PowerShell spawn (Requirement 3.8).
func mapVMMoveProgress(jobPercent int) int {
	return 5 + (jobPercent*90)/100
}

// mapVMExportProgress maps an Export-VM job completion percentage (0-100) onto the
// vm_export_template task progress band 20-85 (export is the bulk of the work).
// Extracted verbatim from the export progress closure (20 + jobPercent*65/100) so the
// mapping stays regression-testable without a live export; behavior-preserving, no
// extra spawn (Requirement 3.8).
func mapVMExportProgress(jobPercent int) int {
	return 20 + (jobPercent*65)/100
}

func (d *Dispatcher) processVMBatchVM(ctx context.Context, batchAction string, target VMBatchTarget) (VMBatchItemResult, *VMInfo) {
	itemAction := "vm_start"
	desiredState := "Running"
	if batchAction == "vm_batch_stop" {
		itemAction = "vm_stop"
		desiredState = "Off"
	}

	item := VMBatchItemResult{
		VMID:   target.VMID,
		VMName: target.VMName,
		Action: itemAction,
		Status: "failed",
		State:  "Unknown",
	}

	// Combined pre-state check + start/stop operation in a SINGLE PowerShell
	// execution. The script reads the current state, verifies the VM name matches
	// the target (flagging a mismatch instead of operating), and performs the op
	// only when the current state differs from the desired state - returning the
	// previous + resulting state, a changed flag, and a controlled op-error string.
	// This eliminates the separate getVMBatchState round-trip, dropping per-VM
	// spawns from ~3 to ~2 (this combined script + the reused handleVMStatus query),
	// while preserving every observable outcome below.
	// vm_batch_stop is the bulk "Power Off" - a hard power-off, like the single
	// vm_stop action (-TurnOff so it needs no Integration Services).
	opCmdlet := map[string]string{"vm_start": "Start-VM", "vm_stop": "Stop-VM -TurnOff -Force"}[itemAction]
	script := fmt.Sprintf(`
$vm = Get-VM -Id '%[1]s' -ErrorAction Stop
$prev = "$($vm.State)"
$name = $vm.Name
$changed = $false
$opError = ""
$mismatch = $false
if ($name -ine '%[2]s') {
    $mismatch = $true
} elseif ($prev -ine '%[3]s') {
    try {
        $vm | %[4]s -ErrorAction Stop
        $changed = $true
    } catch {
        $opError = "$($_.Exception.Message)"
    }
}
[pscustomobject]@{
    name          = $name
    previousState = $prev
    state         = $prev
    changed       = $changed
    opError       = "$opError"
    mismatch      = $mismatch
} | ConvertTo-Json -Compress
`, escapePSSingleQuoted(target.VMID), escapePSSingleQuoted(target.VMName), desiredState, opCmdlet)

	output, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		item.Error = fmt.Sprintf("failed to get current state: %s", extractPSErrorDetail(err))
		return item, nil
	}
	output = trimPowerShellOutput(output)
	var combined vmBatchStateOp
	if jsonErr := json.Unmarshal(output, &combined); jsonErr != nil {
		parseErr := fmt.Errorf("failed to parse VM state: %v", jsonErr)
		item.Error = fmt.Sprintf("failed to get current state: %s", extractPSErrorDetail(parseErr))
		return item, nil
	}

	// Map the parsed combined result into the base VMBatchItemResult (name-mismatch,
	// operation-error, already-in-desired-state, or success). This mapping is pure -
	// it performs no PowerShell spawn and no vmStatus fetch - so its branches are
	// unit-testable in isolation (see batchItemFromStateOp). The followup tells this
	// caller whether to reconcile the post-op full VMInfo into item.State.
	item, followup := batchItemFromStateOp(target, itemAction, desiredState, combined)
	if !followup.fetch {
		return item, nil
	}

	vmStatus, _ := d.getVMBatchVMStatus(ctx, target.VMID)
	if followup.applyState {
		if vmStatus != nil {
			item.State = vmStatus.State
		} else if followup.defaultState != "" {
			item.State = followup.defaultState
		}
	}
	return item, vmStatus
}

// batchStatusFollowup describes how processVMBatchVM must reconcile the post-op
// full VMInfo (fetched via handleVMStatus) into the VMBatchItemResult produced by
// batchItemFromStateOp. It keeps the I/O (the status fetch) in the caller while the
// pure branch mapping lives in the helper.
//
//   - fetch=false            : no status query (name-mismatch path); return as-is.
//   - fetch, applyState=false: query status for progress but keep the read State
//     (already-in-desired-state path).
//   - fetch, applyState=true, defaultState="" : query status and, only if non-nil,
//     overwrite State with the fetched value (operation-error path).
//   - fetch, applyState=true, defaultState!="": query status and overwrite State
//     with the fetched value, or defaultState when the fetch returns nil (success path).
type batchStatusFollowup struct {
	fetch        bool
	applyState   bool
	defaultState string
}

// batchItemFromStateOp maps a parsed vmBatchStateOp (the combined per-VM
// state+operation script result) plus the batch target and itemAction/desiredState
// into the base VMBatchItemResult, WITHOUT spawning PowerShell or fetching the
// post-op VMInfo. It returns the item and a batchStatusFollowup describing the
// status reconciliation the caller must perform.
//
// This is a behavior-preserving extraction of the mapping formerly inlined in
// processVMBatchVM. It gives the name-mismatch, operation-error,
// already-in-desired-state, and success branches direct unit coverage while the
// RunPowerShell call and the handleVMStatus fetch stay in processVMBatchVM (so the
// per-VM spawn structure - and the task-1 Case C count - is unaffected: this helper
// makes no hyperv.RunPowerShell* calls and is outside the
// {processVMBatchVM, getVMBatchState, handleVMStatus} set).
func batchItemFromStateOp(target VMBatchTarget, itemAction, desiredState string, combined vmBatchStateOp) (VMBatchItemResult, batchStatusFollowup) {
	item := VMBatchItemResult{
		VMID:          target.VMID,
		VMName:        target.VMName,
		Action:        itemAction,
		Status:        "failed",
		State:         combined.PreviousState,
		PreviousState: combined.PreviousState,
	}

	// Name mismatch: reproduce the exact error and skip the operation and the
	// status query entirely.
	if combined.Mismatch {
		item.Error = fmt.Sprintf("VM name mismatch for id %s: expected %q, found %q", target.VMID, target.VMName, combined.Name)
		return item, batchStatusFollowup{fetch: false}
	}
	item.VMName = combined.Name

	// Operation attempted but failed: preserve the "failed to start/stop VM: ..."
	// format, then still fetch the full VMInfo and reflect its state when available.
	if combined.OpError != "" {
		item.Error = fmt.Sprintf("failed to %s VM: %s", strings.TrimPrefix(itemAction, "vm_"), combined.OpError)
		return item, batchStatusFollowup{fetch: true, applyState: true}
	}

	// Already in the desired state: ok, not changed. State stays the read value.
	if !combined.Changed {
		item.Status = "ok"
		return item, batchStatusFollowup{fetch: true, applyState: false}
	}

	// Operation succeeded and changed the state: reflect the fetched state, falling
	// back to the desired state when the status query returns nothing.
	item.Status = "ok"
	item.Changed = true
	return item, batchStatusFollowup{fetch: true, applyState: true, defaultState: desiredState}
}

func (d *Dispatcher) getVMBatchVMStatus(ctx context.Context, vmID string) (*VMInfo, error) {
	payload, _ := json.Marshal(map[string]string{"vm_id": vmID})
	inventory, err := d.handleVMStatus(ctx, payload)
	if err != nil {
		return nil, err
	}
	if inventory == nil || len(inventory.VMs) == 0 {
		return nil, fmt.Errorf("VM status not returned for %s", vmID)
	}
	return &inventory.VMs[0], nil
}

func (d *Dispatcher) publishVMBatchProgress(ctx context.Context, taskID string, percent int, message string, result VMBatchItemResult, vmStatus *VMInfo) {
	if d.onProgress == nil {
		return
	}
	response := &TaskResponse{
		TaskID:    taskID,
		HostID:    d.hostID,
		Type:      TaskVMManagement,
		Status:    "in_progress",
		Result:    result,
		Progress:  &Progress{Percent: percent, Message: message},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if vmStatus != nil {
		response.VMStatus = vmStatus
	}
	d.onProgress(ctx, response)
}

func isCanonicalGUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if value[i] != '-' {
				return false
			}
			continue
		}
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func escapePSSingleQuoted(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func (d *Dispatcher) handleVMStartAction(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_start action")
	}

	script := fmt.Sprintf(`Start-VM -Name "%s" -ErrorAction Stop`, p.VMName)
	if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
		detail := extractPSErrorDetail(err)
		return nil, fmt.Errorf("failed to start VM '%s': %s", p.VMName, detail)
	}

	// Get final state
	stateScript := fmt.Sprintf(`(Get-VM -Name "%s").State.ToString()`, p.VMName)
	state, err := hyperv.RunPowerShellRaw(ctx, stateScript)
	if err != nil {
		state = "Running"
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "vm_start",
		Status: state,
	}, nil
}

func (d *Dispatcher) handleVMStopAction(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_stop action")
	}

	// "Turn off" is a hard power-off: -TurnOff pulls the virtual power (no guest
	// involvement, so it works without Integration Services and is near-instant),
	// -Force skips the confirmation prompt. (A graceful guest shutdown is the
	// separate vm_shutdown action.) Single script: stop + wait for Off (30s / 2s
	// poll) + read final state, in one PowerShell execution.
	script := fmt.Sprintf(`
Stop-VM -Name "%[1]s" -TurnOff -Force -ErrorAction SilentlyContinue
$timeout = 30
$elapsed = 0
while ($elapsed -lt $timeout) {
    $state = (Get-VM -Name "%[1]s").State.ToString()
    if ($state -eq "Off") { break }
    Start-Sleep -Seconds 2
    $elapsed += 2
}
$state
`, p.VMName)

	state, _ := hyperv.RunPowerShellRaw(ctx, script)
	return newVMStopResult(p.VMID, p.VMName, state), nil
}

// newVMStopResult builds the vm_stop result payload, defaulting an empty final
// state to "Off". Extracted verbatim from handleVMStopAction as a pure helper so
// the result mapping (including the "Off" default) can be captured as a
// preservation baseline without a live PowerShell host.
func newVMStopResult(vmID, vmName, state string) *VMManagementResult {
	if state == "" {
		state = "Off"
	}
	return &VMManagementResult{
		VMID:   vmID,
		VMName: vmName,
		Action: "vm_stop",
		Status: state,
	}
}

func (d *Dispatcher) handleVMPauseAction(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_pause action")
	}

	script := fmt.Sprintf(`Suspend-VM -Name "%s" -ErrorAction Stop`, p.VMName)
	if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
		return nil, fmt.Errorf("failed to pause VM '%s': %s", p.VMName, extractPSErrorDetail(err))
	}

	state, err := hyperv.RunPowerShellRaw(ctx, fmt.Sprintf(`(Get-VM -Name "%s").State.ToString()`, p.VMName))
	if err != nil {
		state = "Paused"
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "vm_pause",
		Status: state,
	}, nil
}

func (d *Dispatcher) handleVMResumeAction(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_resume action")
	}

	script := fmt.Sprintf(`Resume-VM -Name "%s" -ErrorAction Stop`, p.VMName)
	if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
		return nil, fmt.Errorf("failed to resume VM '%s': %s", p.VMName, extractPSErrorDetail(err))
	}

	state, err := hyperv.RunPowerShellRaw(ctx, fmt.Sprintf(`(Get-VM -Name "%s").State.ToString()`, p.VMName))
	if err != nil {
		state = "Running"
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "vm_resume",
		Status: state,
	}, nil
}

func (d *Dispatcher) handleVMRestartAction(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_restart action")
	}

	script := fmt.Sprintf(`Restart-VM -Name "%s" -Force -ErrorAction Stop`, p.VMName)
	if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
		detail := extractPSErrorDetail(err)
		return nil, fmt.Errorf("failed to restart VM '%s': %s", p.VMName, detail)
	}

	// Get final state
	stateScript := fmt.Sprintf(`(Get-VM -Name "%s").State.ToString()`, p.VMName)
	state, err := hyperv.RunPowerShellRaw(ctx, stateScript)
	if err != nil {
		state = "Running"
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "vm_restart",
		Status: state,
	}, nil
}

func (d *Dispatcher) handleVMDelete(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	var params VMDeleteParams
	if err := p.params(&params); err != nil {
		return nil, err
	}

	// Use a dedicated context - delete can take a long time with large VHDs
	deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer deleteCancel()

	// Find the VM, verify it's Off, get path
	findScript := fmt.Sprintf(`
try {
    $v = Get-VM -Name "%s" -ErrorAction SilentlyContinue
    if (-not $v) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    if ($v.State -ne 'Off') {
        [Console]::Error.WriteLine("VM_NOT_OFF")
        exit 1
    }
    $v | Select-Object Name, Path | ConvertTo-Json -Compress
} catch {
    [Console]::Error.WriteLine("VM_DELETE_CHECK_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName)

	output, err := hyperv.RunPowerShell(deleteCtx, findScript)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM not found: %s", p.VMName)
		}
		if strings.Contains(errStr, "VM_NOT_OFF") {
			return nil, fmt.Errorf("VM must be powered off before deletion")
		}
		return nil, fmt.Errorf("failed to find VM '%s': %s", p.VMName, extractPSErrorDetail(err, "VM_DELETE_CHECK_FAILED:"))
	}

	output = trimPowerShellOutput(output)
	var vmInfo struct {
		Name string `json:"Name"`
		Path string `json:"Path"`
	}
	if err := json.Unmarshal(output, &vmInfo); err != nil {
		return nil, fmt.Errorf("failed to parse VM info: %v", err)
	}

	// Capture the VHD paths while the VM still exists - needed to wipe disks that
	// live outside the VM's config folder when remove_files is set.
	var diskPaths []string
	if params.RemoveFiles {
		diskOut, derr := hyperv.RunPowerShellRaw(deleteCtx, fmt.Sprintf(
			`Get-VM -Name "%s" | Get-VMHardDiskDrive | Select-Object -ExpandProperty Path`, p.VMName))
		if derr != nil {
			d.logger.Warn("vm_delete: could not enumerate VHDs", "vm", p.VMName, "error", derr)
		} else {
			for _, line := range strings.Split(strings.ReplaceAll(diskOut, "\r\n", "\n"), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					diskPaths = append(diskPaths, line)
				}
			}
		}
	}

	// Remove from cluster if HA (only if this host is part of a cluster)
	if d.cluster.IsClusterNode {
		clusterScript := fmt.Sprintf(`
$res = Get-ClusterResource -VMId "%s" -ErrorAction SilentlyContinue
if ($res) {
    Remove-ClusterGroup -VMId "%s" -RemoveResources -Force -ErrorAction SilentlyContinue
}
`, p.VMID, p.VMID)
		hyperv.RunPowerShell(deleteCtx, clusterScript) // best-effort
	}

	// Remove the VM from Hyper-V
	removeScript := fmt.Sprintf(`Get-VM -Name "%s" | Remove-VM -Force -ErrorAction Stop`, p.VMName)
	if _, err := hyperv.RunPowerShell(deleteCtx, removeScript); err != nil {
		return nil, fmt.Errorf("failed to remove VM '%s': %s", p.VMName, extractPSErrorDetail(err))
	}

	result := &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "vm_delete",
		Status: "deleted",
	}

	// Remove files from disk if requested. Deletes the VM's config folder plus any
	// VHD that lives outside it, then prunes an emptied "Virtual Hard Disks"
	// folder. Runs in a background job so a big VHD tree can't stall the worker.
	if params.RemoveFiles {
		targetsPS := psStringArray(append([]string{vmInfo.Path}, diskPaths...))
		removeFilesScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$targets = %s
$job = Start-Job -ScriptBlock {
    param($targets)
    foreach ($t in $targets) {
        if ($t -and (Test-Path -LiteralPath $t)) {
            Remove-Item -LiteralPath $t -Recurse -Force -ErrorAction Stop
        }
    }
    # prune a now-empty "Virtual Hard Disks" folder left behind by a moved VHD
    foreach ($t in $targets) {
        $dir = Split-Path -Parent $t
        if ($dir -and (Split-Path -Leaf $dir) -eq 'Virtual Hard Disks' -and
            (Test-Path -LiteralPath $dir) -and
            -not (Get-ChildItem -LiteralPath $dir -Force)) {
            Remove-Item -LiteralPath $dir -Force -ErrorAction SilentlyContinue
        }
    }
} -ArgumentList (,$targets)
$job | Wait-Job -Timeout 540 | Out-Null
if ($job.State -eq 'Running') {
    $job | Stop-Job; $job | Remove-Job -Force
    throw "File removal timed out"
}
if ($job.State -eq 'Failed') {
    $errMsg = $job | Receive-Job -ErrorAction SilentlyContinue 2>&1
    $job | Remove-Job -Force
    throw "File removal failed: $errMsg"
}
$job | Remove-Job -Force
`, targetsPS)
		removed := true
		if _, err := hyperv.RunPowerShell(deleteCtx, removeFilesScript); err != nil {
			removed = false
			detail := extractPSErrorDetail(err)
			d.logger.Warn("VM removed but failed to delete files", "vm", p.VMName, "path", vmInfo.Path, "error", err)
			result.Warning = fmt.Sprintf("VM deleted from Hyper-V, but some files under %q could not be removed: %s", vmInfo.Path, detail)
		}
		result.FilesRemoved = &removed
	}

	return result, nil
}

// psStringArray renders paths as a PowerShell single-quoted string array literal,
// e.g. @('C:\a','C:\b'). Empty entries are dropped; single quotes are doubled.
func psStringArray(paths []string) string {
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, "'"+strings.ReplaceAll(p, "'", "''")+"'")
		}
	}
	return "@(" + strings.Join(parts, ",") + ")"
}

// SnapshotCreateParams holds the params for the snapshot_create action.
type SnapshotCreateParams struct {
	Name string `json:"name"`
}

func (d *Dispatcher) handleSnapshotCreate(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	var params SnapshotCreateParams
	if err := p.params(&params); err != nil {
		return nil, err
	}

	if params.Name == "" {
		return nil, fmt.Errorf("name is required for snapshot_create")
	}

	script := fmt.Sprintf(`
try {
    $v = Get-VM -Id "%s" -ErrorAction SilentlyContinue
    if (-not $v) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    Checkpoint-VM -VM $v -SnapshotName "%s" -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("SNAPSHOT_CREATE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMID, escapePS(params.Name))

	_, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		if strings.Contains(err.Error(), "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM not found: %s", p.VMID)
		}
		detail := extractPSErrorDetail(err, "SNAPSHOT_CREATE_FAILED:")
		return nil, fmt.Errorf("failed to create snapshot for VM '%s': %s", p.VMName, detail)
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "snapshot_create",
		Status: "completed",
	}, nil
}

// SnapshotIDParams holds the params for the snapshot_remove and snapshot_restore actions.
type SnapshotIDParams struct {
	SnapshotID string `json:"snapshot_id"`
}

func (d *Dispatcher) handleSnapshotRemove(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	var params SnapshotIDParams
	if err := p.params(&params); err != nil {
		return nil, err
	}

	if params.SnapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required for snapshot_remove")
	}

	var script string
	if params.SnapshotID == "all" {
		// Remove all snapshots from the VM
		script = fmt.Sprintf(`
try {
    $v = Get-VM -Id "%s" -ErrorAction SilentlyContinue
    if (-not $v) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    Get-VMSnapshot -VM $v | Remove-VMSnapshot -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("SNAPSHOT_REMOVE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMID)
	} else {
		// Remove a specific snapshot by ID
		script = fmt.Sprintf(`
try {
    $v = Get-VM -Id "%s" -ErrorAction SilentlyContinue
    if (-not $v) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    $snap = Get-VMSnapshot -VM $v | Where-Object { $_.Id -eq "%s" }
    if (-not $snap) {
        [Console]::Error.WriteLine("SNAPSHOT_NOT_FOUND")
        exit 1
    }
    Remove-VMSnapshot -VMSnapshot $snap -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("SNAPSHOT_REMOVE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMID, params.SnapshotID)
	}

	_, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM not found: %s", p.VMID)
		}
		if strings.Contains(errStr, "SNAPSHOT_NOT_FOUND") {
			return nil, fmt.Errorf("snapshot not found: %s", params.SnapshotID)
		}
		detail := extractPSErrorDetail(err, "SNAPSHOT_REMOVE_FAILED:")
		return nil, fmt.Errorf("failed to remove snapshot for VM '%s': %s", p.VMName, detail)
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "snapshot_remove",
		Status: "completed",
	}, nil
}

func (d *Dispatcher) handleSnapshotRestore(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	var params SnapshotIDParams
	if err := p.params(&params); err != nil {
		return nil, err
	}

	if params.SnapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required for snapshot_restore")
	}

	script := fmt.Sprintf(`
try {
    $v = Get-VM -Id "%s" -ErrorAction SilentlyContinue
    if (-not $v) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    $snap = Get-VMSnapshot -VM $v | Where-Object { $_.Id -eq "%s" }
    if (-not $snap) {
        [Console]::Error.WriteLine("SNAPSHOT_NOT_FOUND")
        exit 1
    }
    Restore-VMSnapshot -VMSnapshot $snap -Confirm:$false -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("SNAPSHOT_RESTORE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMID, params.SnapshotID)

	_, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM not found: %s", p.VMID)
		}
		if strings.Contains(errStr, "SNAPSHOT_NOT_FOUND") {
			return nil, fmt.Errorf("snapshot not found: %s", params.SnapshotID)
		}
		detail := extractPSErrorDetail(err, "SNAPSHOT_RESTORE_FAILED:")
		return nil, fmt.Errorf("failed to restore snapshot for VM '%s': %s", p.VMName, detail)
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "snapshot_restore",
		Status: "completed",
	}, nil
}

func (d *Dispatcher) handleRefreshStatus(ctx context.Context, p VMManagementPayload) (*VMInventoryResult, error) {
	// Build payload for handleVMStatus using vm_id (preferred) or vm_name
	var statusPayload []byte
	if p.VMID != "" {
		statusPayload, _ = json.Marshal(map[string]string{"vm_id": p.VMID})
	} else if p.VMName != "" {
		statusPayload, _ = json.Marshal(map[string]string{"vmName": p.VMName})
	} else {
		return nil, fmt.Errorf("vm_id or vm_name is required for refresh_status")
	}

	return d.handleVMStatus(ctx, json.RawMessage(statusPayload))
}

// NotesEditParams holds the params for the notes_edit action.
type NotesEditParams struct {
	Notes string `json:"notes"`
}

func (d *Dispatcher) handleNotesEdit(ctx context.Context, p VMManagementPayload) (*VMInventoryResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for notes_edit action")
	}

	var params NotesEditParams
	if err := p.params(&params); err != nil {
		return nil, err
	}

	// Set notes (works on running VMs)
	script := fmt.Sprintf(`Set-VM -Name "%s" -Notes "%s" -ErrorAction Stop`, p.VMName, escapePS(params.Notes))
	if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
		return nil, fmt.Errorf("failed to set notes on VM '%s': %s", p.VMName, extractPSErrorDetail(err))
	}

	// Return full VM status
	var statusPayload []byte
	if p.VMID != "" {
		statusPayload, _ = json.Marshal(map[string]string{"vm_id": p.VMID})
	} else {
		statusPayload, _ = json.Marshal(map[string]string{"vmName": p.VMName})
	}

	return d.handleVMStatus(ctx, json.RawMessage(statusPayload))
}

// MountDVDParams holds the params for the mount_dvd action.
type MountDVDParams struct {
	Path string `json:"path"`
}

func (d *Dispatcher) handleEjectDVD(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for eject_dvd action")
	}

	// "Eject" only clears the media, it does NOT remove the drive: removing the
	// drive is a hardware change that fails on a running Generation 1 VM (its DVD
	// drive is on the IDE controller). Clearing the path works live on both
	// generations, and mount_dvd re-uses the empty drive on the next mount.
	script := fmt.Sprintf(`
try {
    $dvd = Get-VMDvdDrive -VMName "%s" -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $dvd) {
        [Console]::Error.WriteLine("NO_DVD_DRIVE")
        exit 1
    }
    if (-not $dvd.Path) {
        # Drive already empty - nothing to eject, treat as success (idempotent)
        exit 0
    }
    Set-VMDvdDrive -VMName "%s" -ControllerNumber $dvd.ControllerNumber -ControllerLocation $dvd.ControllerLocation -Path $null -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("EJECT_DVD_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName, p.VMName)

	_, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "NO_DVD_DRIVE") {
			return nil, fmt.Errorf("VM '%s' does not have a DVD drive", p.VMName)
		}
		return nil, fmt.Errorf("failed to remove DVD drive from VM '%s': %s", p.VMName, extractPSErrorDetail(err, "EJECT_DVD_FAILED:"))
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "eject_dvd",
		Status: "completed",
	}, nil
}

func (d *Dispatcher) handleMountDVD(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for mount_dvd action")
	}

	var params MountDVDParams
	if err := p.params(&params); err != nil {
		return nil, err
	}

	if params.Path == "" {
		return nil, fmt.Errorf("path is required for mount_dvd")
	}

	script := fmt.Sprintf(`
try {
    $vm = Get-VM -Name "%[1]s" -ErrorAction Stop
    $dvd = Get-VMDvdDrive -VMName "%[1]s" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($dvd -and $dvd.Path) {
        [Console]::Error.WriteLine("DVD_ALREADY_MOUNTED")
        exit 1
    }
    if ($dvd) {
        # DVD drive exists but empty - just swap the media (works live on gen 1 and 2)
        Set-VMDvdDrive -VMName "%[1]s" -ControllerNumber $dvd.ControllerNumber -ControllerLocation $dvd.ControllerLocation -Path "%[2]s" -ErrorAction Stop
        $dvdDrive = $dvd
    } else {
        # No DVD drive - adding one is a hardware change: a running gen 1 VM (IDE
        # controller) cannot do that, it must be powered off.
        if ($vm.Generation -eq 1 -and $vm.State -ne 'Off') {
            [Console]::Error.WriteLine("GEN1_NEEDS_OFF")
            exit 1
        }
        $dvdDrive = Add-VMDvdDrive -VMName "%[1]s" -Path "%[2]s" -Passthru -ErrorAction Stop
    }
    # Make the DVD the first boot device. Gen 2 uses Set-VMFirmware; gen 1 uses
    # Set-VMBios startup order (Set-VMFirmware throws on a gen 1 VM).
    if ($vm.Generation -eq 2) {
        Set-VMFirmware -VMName "%[1]s" -FirstBootDevice $dvdDrive -ErrorAction Stop
    } else {
        Set-VMBios -VMName "%[1]s" -StartupOrder @('CD','IDE','LegacyNetworkAdapter','Floppy') -ErrorAction Stop
    }
} catch {
    [Console]::Error.WriteLine("MOUNT_DVD_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName, escapePS(params.Path))

	_, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "DVD_ALREADY_MOUNTED") {
			return nil, fmt.Errorf("VM '%s' already has a DVD mounted, eject it first", p.VMName)
		}
		if strings.Contains(errStr, "GEN1_NEEDS_OFF") {
			return nil, fmt.Errorf("VM '%s' is a Generation 1 (BIOS) VM with no DVD drive - it must be powered off to add one", p.VMName)
		}
		return nil, fmt.Errorf("failed to mount DVD on VM '%s': %s", p.VMName, extractPSErrorDetail(err, "MOUNT_DVD_FAILED:"))
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "mount_dvd",
		Status: "completed",
	}, nil
}

func (d *Dispatcher) handleEnableHA(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if !d.cluster.IsClusterNode {
		return nil, fmt.Errorf("action 'enable_ha' requires the host to be part of a cluster")
	}
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for enable_ha action")
	}

	// Check if the VM is already HA (already a cluster resource)
	script := fmt.Sprintf(`
try {
    $res = Get-ClusterResource -VMId "%s" -ErrorAction SilentlyContinue
    if ($res) {
        [Console]::Error.WriteLine("ALREADY_HA")
        exit 1
    }
    Add-ClusterVirtualMachineRole -VMName "%s" -ErrorAction Stop | Out-Null
    # Configure recommended automatic actions
    Get-VM -Name "%s" | Set-VM -AutomaticStartAction StartIfRunning -ErrorAction SilentlyContinue
} catch {
    [Console]::Error.WriteLine("ENABLE_HA_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMID, p.VMName, p.VMName)

	_, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "ALREADY_HA") {
			// Idempotent: VM already has HA - treat as success
			return &VMManagementResult{
				VMID:   p.VMID,
				VMName: p.VMName,
				Action: "enable_ha",
				Status: "completed",
			}, nil
		}
		return nil, fmt.Errorf("failed to enable HA for VM '%s': %s", p.VMName, extractPSErrorDetail(err, "ENABLE_HA_FAILED:"))
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "enable_ha",
		Status: "completed",
	}, nil
}

func (d *Dispatcher) handleDisableHA(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if !d.cluster.IsClusterNode {
		return nil, fmt.Errorf("action 'disable_ha' requires the host to be part of a cluster")
	}
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for disable_ha action")
	}

	// Check if the VM is actually HA before trying to remove
	script := fmt.Sprintf(`
try {
    $res = Get-ClusterResource -VMId "%s" -ErrorAction SilentlyContinue
    if (-not $res) {
        [Console]::Error.WriteLine("NOT_HA")
        exit 1
    }
    Remove-ClusterGroup -VMId "%s" -RemoveResources -Force -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("DISABLE_HA_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMID, p.VMID)

	_, err := hyperv.RunPowerShell(ctx, script)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "NOT_HA") {
			// Idempotent: VM already doesn't have HA - treat as success
			return &VMManagementResult{
				VMID:   p.VMID,
				VMName: p.VMName,
				Action: "disable_ha",
				Status: "completed",
			}, nil
		}
		return nil, fmt.Errorf("failed to disable HA for VM '%s': %s", p.VMName, extractPSErrorDetail(err, "DISABLE_HA_FAILED:"))
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "disable_ha",
		Status: "completed",
	}, nil
}

// VMMigrateParams holds the params for the vm_migrate action.
type VMMigrateParams struct {
	TargetHost string `json:"target_host"`
}

func (d *Dispatcher) handleVMMigrate(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_migrate action")
	}

	var details VMMigrateParams
	if err := p.params(&details); err != nil {
		return nil, err
	}

	if details.TargetHost == "" {
		return nil, fmt.Errorf("target_host is required for vm_migrate")
	}

	// Use a dedicated context - migration can take a long time
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer migrateCancel()

	// If the host is part of a cluster, try cluster-aware migration first.
	// The VM may be on a clustered host but NOT added as a cluster resource (not HA).
	// We validate by checking Get-ClusterGroup and confirming the VM name matches.
	if d.cluster.IsClusterNode {
		// Validate the target host is a known cluster node and is Up
		script := `Get-ClusterNode | Select-Object Name, State | ConvertTo-Json -Compress`
		output, err := hyperv.RunPowerShell(ctx, script)
		if err != nil {
			return nil, fmt.Errorf("failed to query cluster nodes: %s", extractPSErrorDetail(err))
		}

		output = trimPowerShellOutput(output)

		type clusterNodeInfo struct {
			Name  string `json:"Name"`
			State int    `json:"State"`
		}

		var nodes []clusterNodeInfo
		// Handle single object vs array
		if len(output) > 0 && output[0] == '{' {
			var single clusterNodeInfo
			if err := json.Unmarshal(output, &single); err != nil {
				return nil, fmt.Errorf("failed to parse cluster nodes: %v", err)
			}
			nodes = []clusterNodeInfo{single}
		} else {
			if err := json.Unmarshal(output, &nodes); err != nil {
				return nil, fmt.Errorf("failed to parse cluster nodes: %v", err)
			}
		}

		// Find target host in the cluster nodes (case-insensitive)
		var targetFound bool
		var targetState int
		for _, n := range nodes {
			if strings.EqualFold(n.Name, details.TargetHost) {
				targetFound = true
				targetState = n.State
				break
			}
		}

		if !targetFound {
			// Target host is not a cluster node - fall through to non-cluster migration
			d.logger.Info("target host is not a cluster node, using non-cluster live migration",
				"vm", p.VMName, "target", details.TargetHost)
			return d.migrateVMWithoutCluster(migrateCtx, p, details)
		}

		// ClusterNode state 0 = Up
		if targetState != 0 {
			stateNames := map[int]string{0: "Up", 1: "Down", 2: "Paused", 3: "Joining"}
			stateName := stateNames[targetState]
			if stateName == "" {
				stateName = fmt.Sprintf("Unknown(%d)", targetState)
			}
			return nil, fmt.Errorf("target host '%s' is not available for migration, current state: %s", details.TargetHost, stateName)
		}

		// Try cluster-aware migration: check if the VM is actually a cluster group.
		// Get-ClusterGroup by VM name and pipe to Get-VM to confirm it's our VM.
		clusterCheckScript := fmt.Sprintf(`
try {
    $vm = Get-ClusterGroup -Name "%s" -ErrorAction SilentlyContinue | Get-VM -ErrorAction SilentlyContinue
    if ($vm -and $vm.Name -eq "%s") {
        Write-Output "CLUSTER_VM"
    } else {
        Write-Output "NOT_CLUSTER_VM"
    }
} catch {
    Write-Output "NOT_CLUSTER_VM"
}
`, p.VMName, p.VMName)

		checkOutput, err := hyperv.RunPowerShellRaw(migrateCtx, clusterCheckScript)
		if err != nil {
			checkOutput = "NOT_CLUSTER_VM"
		}

		if strings.TrimSpace(checkOutput) == "CLUSTER_VM" {
			// VM is a cluster resource - use Move-ClusterVirtualMachineRole
			migrateScript := fmt.Sprintf(`
try {
    Move-ClusterVirtualMachineRole -Name "%s" -Node "%s" -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("MIGRATE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName, escapePS(details.TargetHost))

			_, err = hyperv.RunPowerShell(migrateCtx, migrateScript)
			if err != nil {
				return nil, fmt.Errorf("failed to migrate VM '%s' to host '%s': %s",
					p.VMName, details.TargetHost, extractPSErrorDetail(err, "MIGRATE_FAILED:"))
			}

			return &VMManagementResult{
				VMID:   p.VMID,
				VMName: p.VMName,
				Action: "vm_migrate",
				Status: fmt.Sprintf("migrated to %s (cluster)", details.TargetHost),
			}, nil
		}

		// VM is NOT a cluster resource - use non-cluster live migration
		d.logger.Info("VM is on a clustered host but not added as a cluster resource, using non-cluster live migration",
			"vm", p.VMName, "target", details.TargetHost)
		return d.migrateVMWithoutCluster(migrateCtx, p, details)
	}

	// Host is not part of a cluster - use non-cluster live migration directly
	return d.migrateVMWithoutCluster(migrateCtx, p, details)
}

// migrateVMWithoutCluster performs a live migration using Move-VM (non-cluster method).
// This works for VMs on standalone hosts or VMs that are not added as cluster resources.
func (d *Dispatcher) migrateVMWithoutCluster(ctx context.Context, p VMManagementPayload, details VMMigrateParams) (*VMManagementResult, error) {
	migrateScript := fmt.Sprintf(`
try {
    $vm = Get-VM -Name "%s" -ErrorAction SilentlyContinue
    if (-not $vm) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    Move-VM -Name "%s" -DestinationHost "%s" -IncludeStorage -ErrorAction Stop
} catch {
    [Console]::Error.WriteLine("MIGRATE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName, p.VMName, escapePS(details.TargetHost))

	_, err := hyperv.RunPowerShell(ctx, migrateScript)
	if err != nil {
		if strings.Contains(err.Error(), "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM '%s' not found on this host", p.VMName)
		}
		return nil, fmt.Errorf("failed to migrate VM '%s' to host '%s': %s",
			p.VMName, details.TargetHost, extractPSErrorDetail(err, "MIGRATE_FAILED:"))
	}

	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "vm_migrate",
		Status: fmt.Sprintf("migrated to %s", details.TargetHost),
	}, nil
}

// VMRenameParams holds the params for the vm_rename action.
type VMRenameParams struct {
	NewName string `json:"new_name"`
}

// VMMoveParams holds the params for the vm_move action.
type VMMoveParams struct {
	DestinationStorage string `json:"destination_storage"`
}

// VMExportTemplateParams holds the params for the vm_export_template action.
type VMExportTemplateParams struct {
	TemplateName string `json:"template_name"`
	Notes        string `json:"notes,omitempty"`
}

// VMExportTemplateResult holds the enriched result for vm_export_template action
type VMExportTemplateResult struct {
	VMID     string        `json:"vmId"`
	VMName   string        `json:"vmName"`
	Action   string        `json:"action"`
	Status   string        `json:"status"`
	Template *TemplateInfo `json:"template"`
}

func (d *Dispatcher) handleVMRename(ctx context.Context, taskID string, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_rename action")
	}

	var details VMRenameParams
	if err := p.params(&details); err != nil {
		return nil, err
	}

	if details.NewName == "" {
		return nil, fmt.Errorf("new_name is required for vm_rename")
	}

	// Use a dedicated context - rename + move can take a long time for large VMs
	renameCtx, renameCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer renameCancel()

	// --- Step 1: Validate VM exists, is Off, and get current path + VHD paths ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 5, "Validating source VM")

	checkScript := fmt.Sprintf(`
try {
    $vm = Get-VM -Name "%s" -ErrorAction SilentlyContinue
    if (-not $vm) {
        $vm = Get-VM -Id "%s" -ErrorAction SilentlyContinue
    }
    if (-not $vm) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    if ($vm.State -ne 'Off') {
        [Console]::Error.WriteLine("VM_NOT_OFF")
        exit 1
    }
    $vm | Select-Object @{N='Name';E={$_.Name}}, @{N='Path';E={$_.Path}}, @{N='Id';E={$_.Id.ToString()}} | ConvertTo-Json -Compress
} catch {
    [Console]::Error.WriteLine("VM_RENAME_CHECK_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName, p.VMID)

	output, err := hyperv.RunPowerShell(renameCtx, checkScript)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM not found: %s", p.VMName)
		}
		if strings.Contains(errStr, "VM_NOT_OFF") {
			return nil, fmt.Errorf("VM '%s' must be powered off before renaming", p.VMName)
		}
		return nil, fmt.Errorf("failed to validate VM '%s': %s", p.VMName, extractPSErrorDetail(err, "VM_RENAME_CHECK_FAILED:"))
	}

	output = trimPowerShellOutput(output)
	var vmInfo struct {
		Name string `json:"Name"`
		Path string `json:"Path"`
		ID   string `json:"Id"`
	}
	if err := json.Unmarshal(output, &vmInfo); err != nil {
		return nil, fmt.Errorf("failed to parse VM info: %v", err)
	}

	// --- Step 2: Determine destination path and validate it doesn't exist ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 10, "Checking destination path")

	// Build destination path: same parent directory as current VM, with the new name
	parentPath := filepath.Dir(vmInfo.Path)
	destPath := filepath.Join(parentPath, details.NewName)

	// Validate destination folder does not already exist
	if _, err := os.Stat(destPath); err == nil {
		return nil, fmt.Errorf("Folder already exists: %s", destPath)
	}

	// --- Step 3: Rename the VM ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 15, "Renaming VM")

	renameScript := fmt.Sprintf(`
try {
    Rename-VM "%s" -NewName "%s" -Passthru -ErrorAction Stop | Out-Null
} catch {
    [Console]::Error.WriteLine("VM_RENAME_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName, escapePS(details.NewName))

	_, err = hyperv.RunPowerShell(renameCtx, renameScript)
	if err != nil {
		return nil, fmt.Errorf("failed to rename VM '%s' to '%s': %s", p.VMName, details.NewName, extractPSErrorDetail(err, "VM_RENAME_FAILED:"))
	}

	// --- Step 4: Move VM storage using AsJob for progress reporting ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 25, "Moving VM storage")

	moveScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
try {
    $job = Move-VMStorage -VMName "%s" -DestinationStoragePath "%s" -AsJob
    while ($job.State -eq 'Running') {
        $pct = $job.Progress | Select-Object -Last 1 | ForEach-Object { $_.PercentComplete }
        if ($pct -and $pct -gt 0) {
            Write-Output "PROGRESS:$pct"
        }
        Start-Sleep -Seconds 5
    }
    if ($job.State -eq 'Failed') {
        $errMsg = $job.ChildJobs[0].JobStateInfo.Reason.Message
        if (-not $errMsg) { $errMsg = ($job | Receive-Job 2>&1) }
        Remove-Job $job -Force -ErrorAction SilentlyContinue
        throw "Move-VMStorage failed: $errMsg"
    }
    Remove-Job $job -Force -ErrorAction SilentlyContinue
} catch {
    [Console]::Error.WriteLine("VM_MOVE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, escapePS(details.NewName), destPath)

	_, err = hyperv.RunPowerShell(renameCtx, moveScript)
	if err != nil {
		// Cleanup destination directory if it was left empty after failure
		cleanupEmptyDir(destPath, d.logger)
		return nil, fmt.Errorf("failed to move VM '%s' to '%s': %s", details.NewName, destPath, extractPSErrorDetail(err, "VM_MOVE_FAILED:"))
	}

	// --- Step 5: Cleanup old directory if empty ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 95, "Cleaning up old directory")
	cleanupEmptyDir(vmInfo.Path, d.logger)

	vmIDResult := vmInfo.ID
	if p.VMID != "" {
		vmIDResult = p.VMID
	}

	d.reportProgress(ctx, taskID, TaskVMManagement, 100, "Rename completed")

	return &VMManagementResult{
		VMID:   vmIDResult,
		VMName: details.NewName,
		Action: "vm_rename",
		Status: "completed",
	}, nil
}

// resolveStandaloneMoveDestination validates a vm_move destination on a standalone
// host and returns the path to use as the migration destination.
//
// Allowed destinations are:
//   - The Hyper-V default VM path (Get-VMHost.VirtualMachinePath) - accepted as-is.
//   - Any configured (and startup-validated) aditional_vm_storage root - the normalized
//     configured root is returned so the VM lands under an existing "HyperV" folder.
//
// Matching accepts either an exact normalized-path match or a same-volume match, so a
// caller may pass "E:", "E:\\", or "E:\\HyperV" for a configured "E:\\HyperV" root, or
// the default drive letter for the default VM path.
func (d *Dispatcher) resolveStandaloneMoveDestination(ctx context.Context, destinationStorage string) (string, error) {
	// Normalize the requested destination the same way aditional_vm_storage roots are
	// normalized in the config package: clean it and ensure a "HyperV" leaf folder.
	requested := destinationStorage
	if !strings.EqualFold(filepath.Base(filepath.Clean(requested)), "HyperV") {
		requested = filepath.Join(requested, "HyperV")
	}
	requested = filepath.Clean(requested)
	reqVolume := strings.ToUpper(filepath.VolumeName(requested))

	// Allow the Hyper-V default VM path (same volume as the host's VirtualMachinePath).
	// The destination is accepted as requested so the VM lands under the default drive.
	if defaultPath := d.getDefaultVMPath(ctx); defaultPath != "" {
		if strings.EqualFold(filepath.Clean(defaultPath), requested) {
			return requested, nil
		}
		if reqVolume != "" && strings.EqualFold(strings.ToUpper(filepath.VolumeName(defaultPath)), reqVolume) {
			return requested, nil
		}
	}

	// Otherwise the destination must match a configured aditional_vm_storage root.
	for _, root := range d.additionalStorage {
		if strings.EqualFold(filepath.Clean(root), requested) {
			return root, nil
		}
		if reqVolume != "" && strings.EqualFold(strings.ToUpper(filepath.VolumeName(root)), reqVolume) {
			return root, nil
		}
	}

	if len(d.additionalStorage) == 0 {
		return "", fmt.Errorf(
			"destination_storage %q is not allowed: it is not the default VM path and no aditional_vm_storage is configured",
			destinationStorage)
	}
	return "", fmt.Errorf(
		"destination_storage %q is not the default VM path nor a configured aditional_vm_storage location (configured: %s)",
		destinationStorage, strings.Join(d.additionalStorage, ", "))
}

// getDefaultVMPath returns the Hyper-V host default virtual machine path
// (Get-VMHost.VirtualMachinePath), or "" if it cannot be determined.
func (d *Dispatcher) getDefaultVMPath(ctx context.Context) string {
	out, err := hyperv.RunPowerShellRaw(ctx, `(Get-VMHost).VirtualMachinePath`)
	if err != nil {
		d.logger.Warn("vm_move: failed to query default VM path", "error", err)
		return ""
	}
	return strings.TrimSpace(out)
}

func (d *Dispatcher) handleVMMove(ctx context.Context, taskID string, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_move action")
	}

	var details VMMoveParams
	if err := p.params(&details); err != nil {
		return nil, err
	}

	details.DestinationStorage = strings.TrimSpace(details.DestinationStorage)
	if details.DestinationStorage == "" {
		return nil, fmt.Errorf("destination_storage is required for vm_move")
	}

	// Storage migration is available for both clustered and standalone hosts.
	//
	// On CLUSTER hosts the destination is a Cluster Shared Volume path and is accepted
	// as-is (the previous behavior).
	//
	// On STANDALONE hosts, storage migration is only allowed for whitelisted destinations:
	// the Hyper-V default VM path, or one of the configured (and startup-validated)
	// aditional_vm_storage roots. When the destination matches a configured additional
	// root, we use that normalized root so the VM lands under an existing "HyperV" folder.
	// Use a dedicated context - move can take a long time for large VMs
	moveCtx, moveCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer moveCancel()

	if !d.cluster.IsClusterNode {
		resolved, err := d.resolveStandaloneMoveDestination(moveCtx, details.DestinationStorage)
		if err != nil {
			return nil, err
		}
		details.DestinationStorage = resolved
	}

	// --- Step 1: Validate VM exists ---
	// The VM does NOT need to be powered off: Move-VMStorage performs a live storage
	// migration, keeping the VM running during the copy and switching over at the end.
	d.reportProgress(ctx, taskID, TaskVMManagement, 1, "Validating VM state")

	checkScript := fmt.Sprintf(`
try {
    $vm = Get-VM -Name "%s" -ErrorAction SilentlyContinue
    if (-not $vm) {
        $vm = Get-VM -Id "%s" -ErrorAction SilentlyContinue
    }
    if (-not $vm) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    $vm | Select-Object @{N='Name';E={$_.Name}}, @{N='Path';E={$_.Path}}, @{N='Id';E={$_.Id.ToString()}}, @{N='State';E={$_.State.ToString()}} | ConvertTo-Json -Compress
} catch {
    [Console]::Error.WriteLine("VM_MOVE_CHECK_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName, p.VMID)

	output, err := hyperv.RunPowerShell(moveCtx, checkScript)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM not found: %s", p.VMName)
		}
		return nil, fmt.Errorf("failed to validate VM '%s': %s", p.VMName, extractPSErrorDetail(err, "VM_MOVE_CHECK_FAILED:"))
	}

	output = trimPowerShellOutput(output)
	var vmInfo struct {
		Name  string `json:"Name"`
		Path  string `json:"Path"`
		ID    string `json:"Id"`
		State string `json:"State"`
	}
	if err := json.Unmarshal(output, &vmInfo); err != nil {
		return nil, fmt.Errorf("failed to parse VM info: %v", err)
	}

	// --- Step 2: Ensure destination storage ends with a "HyperV" folder ---
	// If the path does not already contain "hyper" (case-insensitive), append "HyperV".
	// This guarantees VMs always land under a HyperV subfolder, e.g.:
	//   C:\ClusterStorage\Volume2\HyperV\VM-NAME
	destinationStorage := details.DestinationStorage
	if !strings.Contains(strings.ToLower(destinationStorage), "hyper") {
		destinationStorage = filepath.Join(destinationStorage, "HyperV")
	}

	// --- Step 3: Validate destination folder doesn't already exist ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 3, "Checking destination path")

	destPath := filepath.Join(destinationStorage, vmInfo.Name)

	if _, err := os.Stat(destPath); err == nil {
		return nil, fmt.Errorf("Folder already exists: %s", destPath)
	}

	// --- Step 4: Move VM storage (may take several minutes for large VMs) ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 5, "Moving VM storage (this may take a while)")

	d.logger.Info("vm_move: starting Move-VMStorage",
		"vm", p.VMName, "source_path", vmInfo.Path, "dest_path", destPath,
		"vm_state", vmInfo.State, "is_cluster_node", d.cluster.IsClusterNode)

	// Determine if this VM is a cluster resource (HA). If so, we must pipe through
	// Get-ClusterGroup to get the correct VM object for Move-VMStorage in cluster contexts.
	var isClusterVM bool
	if d.cluster.IsClusterNode {
		haCheckScript := fmt.Sprintf(`
$res = Get-ClusterResource -VMId "%s" -ErrorAction SilentlyContinue
if ($res) { Write-Output "YES" } else { Write-Output "NO" }
`, p.VMID)
		haResult, _ := hyperv.RunPowerShellRaw(moveCtx, haCheckScript)
		isClusterVM = strings.TrimSpace(haResult) == "YES"
		d.logger.Info("vm_move: HA check", "vm", p.VMName, "is_cluster_vm", isClusterVM)
	}

	// Build the pipeline that resolves the VM object. Cluster (HA) VMs must go through
	// Get-ClusterGroup so Move-VMStorage runs in the cluster context.
	var vmResolver string
	if isClusterVM {
		vmResolver = fmt.Sprintf(`Get-ClusterGroup -VMId "%s" -ErrorAction Stop | Get-VM -ErrorAction Stop`, p.VMID)
	} else {
		vmResolver = fmt.Sprintf(`Get-VM -Name "%s" -ErrorAction Stop`, p.VMName)
	}

	// Run Move-VMStorage as a job INSIDE a single PowerShell process. The script polls
	// its own job and emits PROGRESS:nn lines, which we stream back in Go for live updates.
	// [Console]::Out.WriteLine + Flush bypasses PowerShell's output buffering so lines
	// arrive immediately instead of all at once at the end.
	moveScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
try {
    $vm = %s
    if (-not $vm) {
        throw "VM not found: %s"
    }
    [Console]::Out.WriteLine("BEFORE_PATH:$($vm.Path)")
    [Console]::Out.Flush()

    $job = $vm | Move-VMStorage -DestinationStoragePath "%s" -AsJob
    if (-not $job) {
        throw "Move-VMStorage did not return a job"
    }

    $jobId = $job.Id
    $lastPct = -1
    while ($true) {
        # Refresh the job object on every poll. Move-VMStorage publishes progress on
        # the parent job itself, equivalent to:
        # (Get-Job -Id $jobId).Progress[-1].PercentComplete
        $job = Get-Job -Id $jobId -ErrorAction Stop
        $pct = $null
        try {
            if ($job.Progress -and $job.Progress.Count -gt 0) {
                $pct = [int]$job.Progress[-1].PercentComplete
            }
        } catch {}

        if ($null -ne $pct -and $pct -ge 0 -and $pct -ne $lastPct) {
            $lastPct = $pct
            [Console]::Out.WriteLine("PROGRESS:$pct")
            [Console]::Out.Flush()
        }

        if ($job.State -ne 'Running' -and $job.State -ne 'NotStarted') {
            break
        }
        Start-Sleep -Seconds 3
    }

    if ($job.State -ne 'Completed') {
        $errMsg = ""
        try { $errMsg = $job.ChildJobs[0].JobStateInfo.Reason.Message } catch {}
        if (-not $errMsg) {
            try { $errMsg = ($job | Receive-Job 2>&1 | Out-String).Trim() } catch {}
        }
        if (-not $errMsg) { $errMsg = "job ended in state $($job.State)" }
        Remove-Job $job -Force -ErrorAction SilentlyContinue
        throw "Move-VMStorage failed: $errMsg"
    }

    Remove-Job $job -Force -ErrorAction SilentlyContinue

    # Verify the VM configuration path actually changed
    $vmAfter = Get-VM -Name "%s" -ErrorAction Stop
    [Console]::Out.WriteLine("AFTER_PATH:$($vmAfter.Path)")
    [Console]::Out.Flush()

    $expectedPath = "%s".TrimEnd('\')
    $actualPath = $vmAfter.Path.TrimEnd('\')
    if ($actualPath -ne $expectedPath) {
        throw "Move completed but VM path did not change. Expected: $expectedPath, Got: $actualPath"
    }
} catch {
    [Console]::Error.WriteLine("VM_MOVE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, vmResolver, p.VMName, destPath, p.VMName, destPath)

	// Run storage migration in the isolated long-operation lane. It remains serialized
	// against other long operations but does not block periodic inventory PowerShell calls.
	lastReported := 5
	moveOutput, err := hyperv.RunLongPowerShellStream(moveCtx, moveScript, func(line string) {
		if !strings.HasPrefix(line, "PROGRESS:") {
			return
		}
		var jobPercent int
		if _, scanErr := fmt.Sscanf(strings.TrimPrefix(line, "PROGRESS:"), "%d", &jobPercent); scanErr != nil {
			return
		}
		mapped := mapVMMoveProgress(jobPercent)
		if mapped > lastReported {
			lastReported = mapped
			d.reportProgress(ctx, taskID, TaskVMManagement, mapped,
				fmt.Sprintf("Moving VM storage (%d%%)", jobPercent))
		}
	})
	if err != nil {
		d.logger.Error("vm_move: Move-VMStorage failed",
			"vm", p.VMName, "dest_path", destPath, "error", err)
		cleanupEmptyDir(destPath, d.logger)
		return nil, fmt.Errorf("failed to move VM '%s' to '%s': %s", p.VMName, destPath, extractPSErrorDetail(err, "VM_MOVE_FAILED:"))
	}

	if len(moveOutput) > 0 {
		d.logger.Debug("vm_move: Move-VMStorage output", "vm", p.VMName, "output", string(moveOutput))
	}
	d.logger.Info("vm_move: Move-VMStorage completed successfully",
		"vm", p.VMName, "dest_path", destPath)

	// --- Step 5: Cleanup old directory if empty ---
	cleanupEmptyDir(vmInfo.Path, d.logger)

	d.reportProgress(ctx, taskID, TaskVMManagement, 100, "Move completed")

	vmIDResult := vmInfo.ID
	if p.VMID != "" {
		vmIDResult = p.VMID
	}

	return &VMManagementResult{
		VMID:   vmIDResult,
		VMName: p.VMName,
		Action: "vm_move",
		Status: "completed",
	}, nil
}

// cleanupEmptyDir recursively removes a directory tree if it contains no files.
// It only removes directories that are empty (no files at any level), which is the
// expected state after Move-VMStorage has moved all VM files to the new location.
// This is best-effort - errors are logged but do not fail the task.
func cleanupEmptyDir(dir string, logger *slog.Logger) {
	if dir == "" {
		return
	}

	// Check if the directory exists
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}

	// Walk the directory tree to check for any files
	hasFiles := false
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			hasFiles = true
			return filepath.SkipAll
		}
		return nil
	})

	if hasFiles {
		logger.Info("vm_rename/vm_move: source directory still has files, skipping cleanup", "path", dir)
		return
	}

	// Directory tree is empty - remove it
	if err := os.RemoveAll(dir); err != nil {
		logger.Warn("vm_rename/vm_move: failed to remove empty source directory", "path", dir, "error", err)
	} else {
		logger.Info("vm_rename/vm_move: removed empty source directory", "path", dir)
	}
}

// VMStartupChangeParams holds the params for the vm_startup_change action.
type VMStartupChangeParams struct {
	AutomaticStart      *string `json:"automatic_start"`
	AutomaticStartDelay *int    `json:"automatic_start_delay"`
}

func (d *Dispatcher) handleVMStartupChange(ctx context.Context, p VMManagementPayload) (*VMInventoryResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_startup_change action")
	}

	var details VMStartupChangeParams
	if err := p.params(&details); err != nil {
		return nil, err
	}

	if details.AutomaticStart == nil && details.AutomaticStartDelay == nil {
		return nil, fmt.Errorf("at least one of 'automatic_start' or 'automatic_start_delay' is required for vm_startup_change")
	}

	// Build Set-VM command with the specified parameters
	params := ""
	if details.AutomaticStart != nil {
		validActions := map[string]bool{"Nothing": true, "StartIfRunning": true, "Start": true}
		if !validActions[*details.AutomaticStart] {
			return nil, fmt.Errorf("invalid automatic_start: %q (valid: Nothing, StartIfRunning, Start)", *details.AutomaticStart)
		}
		params += fmt.Sprintf(` -AutomaticStartAction %s`, *details.AutomaticStart)
	}
	if details.AutomaticStartDelay != nil {
		if *details.AutomaticStartDelay < 0 {
			return nil, fmt.Errorf("invalid automatic_start_delay: %d (must be >= 0)", *details.AutomaticStartDelay)
		}
		params += fmt.Sprintf(` -AutomaticStartDelay %d`, *details.AutomaticStartDelay)
	}

	script := fmt.Sprintf(`Set-VM -Name "%s"%s -ErrorAction Stop`, p.VMName, params)
	if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
		return nil, fmt.Errorf("failed to change startup settings for VM '%s': %s", p.VMName, extractPSErrorDetail(err))
	}

	// Return full VM status
	var statusPayload []byte
	if p.VMID != "" {
		statusPayload, _ = json.Marshal(map[string]string{"vm_id": p.VMID})
	} else {
		statusPayload, _ = json.Marshal(map[string]string{"vmName": p.VMName})
	}

	return d.handleVMStatus(ctx, json.RawMessage(statusPayload))
}

// handleVMResourceMetering toggles Hyper-V resource metering on a VM. When on,
// the VM starts appearing in <hostid>.vm_metrics and the next vm_inventory
// reports metricsEnabled: true (the dispatcher refreshes inventory after every
// vm_management task).
func (d *Dispatcher) handleVMResourceMetering(ctx context.Context, p VMManagementPayload, enable bool) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for %s action", p.Action)
	}

	cmdlet := "Enable-VMResourceMetering"
	if !enable {
		cmdlet = "Disable-VMResourceMetering"
	}

	script := fmt.Sprintf(`Get-VM -Name "%s" | %s -ErrorAction Stop`, p.VMName, cmdlet)
	if _, err := hyperv.RunPowerShell(ctx, script); err != nil {
		return nil, fmt.Errorf("failed to run %s for VM '%s': %s", cmdlet, p.VMName, extractPSErrorDetail(err))
	}

	status := "disabled"
	if enable {
		status = "enabled"
	}
	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: p.Action,
		Status: status,
	}, nil
}

func (d *Dispatcher) handleVMExportTemplate(ctx context.Context, taskID string, p VMManagementPayload) (*VMExportTemplateResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_export_template action")
	}

	var details VMExportTemplateParams
	if err := p.params(&details); err != nil {
		return nil, err
	}

	if details.TemplateName == "" {
		return nil, fmt.Errorf("template_name is required for vm_export_template")
	}

	if d.templatePath == "" {
		return nil, fmt.Errorf("template_path is not configured in agent config")
	}

	// --- Step 1: Validate source VM is Off and get VHD paths ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 5, "Validating source VM state")

	checkScript := fmt.Sprintf(`
try {
    $vm = Get-VM -Name "%s" -ErrorAction SilentlyContinue
    if (-not $vm) {
        [Console]::Error.WriteLine("VM_NOT_FOUND")
        exit 1
    }
    if ($vm.State -ne 'Off') {
        [Console]::Error.WriteLine("VM_NOT_OFF")
        exit 1
    }
    # Return VHD paths for native size calculation
    $disks = @($vm | Get-VMHardDiskDrive | ForEach-Object { $_.Path })
    $disks | ConvertTo-Json -Compress
} catch {
    [Console]::Error.WriteLine("EXPORT_TEMPLATE_FAILED: $($_.Exception.Message)")
    exit 1
}
`, p.VMName)

	output, err := hyperv.RunPowerShell(ctx, checkScript)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "VM_NOT_FOUND") {
			return nil, fmt.Errorf("VM not found: %s", p.VMName)
		}
		if strings.Contains(errStr, "VM_NOT_OFF") {
			return nil, fmt.Errorf("VM '%s' must be powered off before exporting as template", p.VMName)
		}
		return nil, fmt.Errorf("failed to validate source VM '%s': %s", p.VMName, extractPSErrorDetail(err, "EXPORT_TEMPLATE_FAILED:"))
	}

	// Parse VHD paths
	output = trimPowerShellOutput(output)
	var vhdPaths []string
	if len(output) > 0 {
		if output[0] == '[' {
			json.Unmarshal(output, &vhdPaths)
		} else {
			// Single disk - PS returns a bare string
			var single string
			if err := json.Unmarshal(output, &single); err == nil {
				vhdPaths = []string{single}
			}
		}
	}

	// --- Step 2: Calculate disk sizes natively (os.Stat) ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 10, "Calculating disk sizes")

	var totalSizeBytes int64
	for _, path := range vhdPaths {
		info, err := os.Stat(path)
		if err != nil {
			d.logger.Warn("vm_export_template: cannot stat VHD, skipping size check", "path", path, "error", err)
			continue
		}
		totalSizeBytes += info.Size()
	}

	totalDiskGB := float64(totalSizeBytes) / (1024 * 1024 * 1024)

	// --- Step 3: Check destination doesn't exist ---
	destPath := d.templatePath + "\\" + details.TemplateName
	if _, err := os.Stat(destPath); err == nil {
		return nil, fmt.Errorf("template directory '%s' already exists at template_path", details.TemplateName)
	}

	// --- Step 4: Disk space check (uses CSV query for cluster, native for standalone) ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 15, "Checking available disk space")

	if totalDiskGB > 0 {
		if err := d.checkDiskSpace(ctx, totalDiskGB, destPath); err != nil {
			return nil, err
		}
	}

	// --- Step 5: Export the VM with progress reporting ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 20, "Exporting VM (0%)")

	exportCtx, exportCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer exportCancel()

	exportScript := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'

# Export VM as a job and poll progress
$job = Export-VM -Name "%s" -Path "%s" -AsJob
if (-not $job) {
    throw "Export-VM did not return a job"
}

$jobId = $job.Id
$lastPct = -1
while ($true) {
    $job = Get-Job -Id $jobId -ErrorAction Stop
    $pct = $null
    try {
        if ($job.Progress -and $job.Progress.Count -gt 0) {
            $pct = [int]$job.Progress[-1].PercentComplete
        }
    } catch {}

    if ($null -ne $pct -and $pct -ge 0 -and $pct -ne $lastPct) {
        $lastPct = $pct
        [Console]::Out.WriteLine("PROGRESS:$pct")
        [Console]::Out.Flush()
    }

    if ($job.State -ne 'Running' -and $job.State -ne 'NotStarted') {
        break
    }
    Start-Sleep -Seconds 3
}

if ($job.State -ne 'Completed') {
    $errMsg = ""
    try { $errMsg = $job.ChildJobs[0].JobStateInfo.Reason.Message } catch {}
    if (-not $errMsg) {
        try { $errMsg = ($job | Receive-Job 2>&1 | Out-String).Trim() } catch {}
    }
    if (-not $errMsg) { $errMsg = "job ended in state $($job.State)" }
    Remove-Job $job -Force -ErrorAction SilentlyContinue
    throw "Export failed: $errMsg"
}
Remove-Job $job -Force -ErrorAction SilentlyContinue

# Rename exported folder from source VM name to template name
$exportedPath = Join-Path "%s" "%s"
$templateDestPath = "%s"

if ($exportedPath -ne $templateDestPath) {
    Rename-Item -Path $exportedPath -NewName "%s" -ErrorAction Stop
}
`, p.VMName, d.templatePath, d.templatePath, p.VMName, destPath, details.TemplateName)

	lastExportPercent := 20
	_, err = hyperv.RunLongPowerShellStream(exportCtx, exportScript, func(line string) {
		if !strings.HasPrefix(line, "PROGRESS:") {
			return
		}
		var jobPercent int
		if _, scanErr := fmt.Sscanf(strings.TrimPrefix(line, "PROGRESS:"), "%d", &jobPercent); scanErr != nil {
			return
		}
		// Map export job 0-100% to task progress 20-85% (export is the bulk of the work)
		mapped := mapVMExportProgress(jobPercent)
		if mapped > lastExportPercent {
			lastExportPercent = mapped
			d.reportProgress(ctx, taskID, TaskVMManagement, mapped,
				fmt.Sprintf("Exporting VM (%d%%)", jobPercent))
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to export VM '%s' as template: %s", p.VMName, extractPSErrorDetail(err))
	}

	// --- Step 5a: Shrink the template - convert any Fixed disks to Dynamic so the
	// template occupies less space in the template store ---
	d.convertExportedTemplateDisksToDynamic(exportCtx, taskID, destPath)

	// --- Step 5b: Build the template metadata (written to disk in step 6b, once
	// the provisioned disk size from step 6 is known) ---
	templateID := generateUUID()
	hostname, _ := os.Hostname()
	metadata := TemplateMetadata{
		ID:         templateID,
		Name:       details.TemplateName,
		Notes:      details.Notes,
		SourceVM:   p.VMName,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		ExportedBy: hostname,
	}

	// --- Step 6: Read template details + provisioned disk size from the exported .vmcx ---
	d.reportProgress(ctx, taskID, TaskVMManagement, 90, "Reading template details")

	var templateInfo *TemplateInfo
	readTemplateScript := fmt.Sprintf(templateSizePSFunc+`
$ErrorActionPreference = 'Stop'
$templateFolder = "%[1]s"
$vmcxFile = (Get-ChildItem "%[1]s\Virtual Machines" -Filter *.vmcx -Recurse | Select-Object -First 1)
if ($vmcxFile) {
    $report = Compare-VM -Copy -GenerateNewId -Path $vmcxFile.FullName -ErrorAction Stop
    $vm = $report.VM
    # Provisioned size = sum of each base disk's virtual (max) size. Scanned from
    # the exported folder - Compare-VM's HardDrives paths point at the (not-yet-
    # existing) import destination, so Get-VHD on them returns nothing.
    $sizeBytes = Get-OvcTemplateSize -TemplateFolder $templateFolder
    $result = [pscustomobject]@{
        name      = $vm.Name
        cpuCount  = $vm.ProcessorCount
        memoryMb  = [math]::Round($vm.MemoryStartup / 1MB)
        notes     = [string]$vm.Notes
        sizeBytes = $sizeBytes
    }
    try { Remove-VM -VM $vm -Force -ErrorAction SilentlyContinue } catch {}
    $result | ConvertTo-Json -Compress
}
`, destPath)

	templateOutput, err := hyperv.RunPowerShell(exportCtx, readTemplateScript)
	if err != nil {
		d.logger.Warn("vm_export_template: export succeeded but failed to read template details", "error", err)
	} else {
		templateOutput = trimPowerShellOutput(templateOutput)
		if len(templateOutput) > 0 {
			var ti TemplateInfo
			if err := json.Unmarshal(templateOutput, &ti); err != nil {
				d.logger.Warn("vm_export_template: failed to parse template details", "error", err)
			} else {
				// Compare-VM reads $vm.Name off the exported .vmcx, which still
				// carries the source VM's name (the folder rename doesn't touch
				// it). Use the name the user configured for the template so the
				// task result matches the later inventory scan.
				ti.ID = templateID
				ti.Name = details.TemplateName
				ti.Path = destPath
				ti.CreatedAt = metadata.ExportedAt
				ti.DiskSizeBytes = d.templateDiskSize(destPath)
				if details.Notes != "" {
					ti.Notes = details.Notes
				}
				metadata.Size = ti.SizeBytes
				templateInfo = &ti
			}
		}
	}

	// --- Step 6b: Write ovc-template-metadata.json (carries the provisioned size
	// so the inventory does not have to re-run Get-VHD) ---
	metaData, _ := json.MarshalIndent(metadata, "", "  ")
	metaPath := destPath + "\\Virtual Machines\\" + templateMetadataFile
	if err := os.WriteFile(metaPath, metaData, 0644); err != nil {
		d.logger.Warn("vm_export_template: failed to write metadata file", "path", metaPath, "error", err)
		// Not fatal - template was exported successfully, ID will fallback to .vmcx name
	}

	d.reportProgress(ctx, taskID, TaskVMManagement, 100, "Export completed")

	return &VMExportTemplateResult{
		VMID:     p.VMID,
		VMName:   p.VMName,
		Action:   "vm_export_template",
		Status:   "exported",
		Template: templateInfo,
	}, nil
}

// logDiskConversionLine routes one status line emitted by the Fixed→Dynamic disk
// conversion script (vm_export_template) to the agent log.
func logDiskConversionLine(logger *slog.Logger, op, line string) {
	switch {
	case strings.HasPrefix(line, "CONVERTED:"):
		logger.Info(op+": disk converted", "disk", strings.TrimSpace(strings.TrimPrefix(line, "CONVERTED:")))
	case strings.HasPrefix(line, "CONVERT_FAILED:"):
		logger.Warn(op+": disk conversion failed, original disk kept", "detail", strings.TrimSpace(strings.TrimPrefix(line, "CONVERT_FAILED:")))
	case strings.HasPrefix(line, "CONVERT_SKIPPED:"):
		logger.Warn(op+": disk conversion skipped, original disk kept", "detail", strings.TrimSpace(strings.TrimPrefix(line, "CONVERT_SKIPPED:")))
	case strings.HasPrefix(line, "SKIP_ALL:"):
		logger.Warn(op+": disk conversion skipped for the whole set", "detail", strings.TrimSpace(strings.TrimPrefix(line, "SKIP_ALL:")))
	}
}

// convertExportedTemplateDisksToDynamic rewrites every Fixed-type .vhd/.vhdx in a
// freshly exported template directory as a Dynamic disk of the same name, so the
// template occupies less space in the template store. Disks that are already
// Dynamic are left untouched. The pass is skipped entirely when the export
// contains checkpoint (.avhdx / differencing) disks, since converting a
// differencing chain's parent is unsafe. Best-effort and non-fatal: a per-disk
// failure is logged and its Fixed disk is kept.
func (d *Dispatcher) convertExportedTemplateDisksToDynamic(ctx context.Context, taskID, dir string) {
	d.reportProgress(ctx, taskID, TaskVMManagement, 87, "Converting fixed disks to dynamic")

	script := fmt.Sprintf(`
$root = "%s"
$all = Get-ChildItem -LiteralPath $root -Recurse -File -ErrorAction SilentlyContinue |
    Where-Object { $_.Extension -eq '.vhd' -or $_.Extension -eq '.vhdx' -or $_.Extension -eq '.avhdx' }
if (@($all | Where-Object { $_.Extension -eq '.avhdx' }).Count -gt 0) {
    [Console]::Out.WriteLine("SKIP_ALL: export contains checkpoint (differencing) disks")
    return
}
foreach ($f in @($all | Where-Object { $_.Extension -ne '.avhdx' })) {
    $tmp = $null
    try {
        $info = Get-VHD -Path $f.FullName -ErrorAction Stop
        if ($info.VhdType -ne 'Fixed') { continue }
        $tmp = Join-Path $f.DirectoryName ('_ovc_conv_' + $f.Name)
        if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force -ErrorAction Stop }
        Convert-VHD -Path $f.FullName -DestinationPath $tmp -VHDType Dynamic -ErrorAction Stop
        Remove-Item -LiteralPath $f.FullName -Force -ErrorAction Stop
        Rename-Item -LiteralPath $tmp -NewName $f.Name -ErrorAction Stop
        [Console]::Out.WriteLine("CONVERTED:" + $f.Name)
    } catch {
        if ($tmp -and (Test-Path -LiteralPath $tmp)) { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue }
        [Console]::Out.WriteLine("CONVERT_FAILED:" + $f.Name + ": " + $_.Exception.Message)
    }
}
`, dir)

	if _, err := hyperv.RunLongPowerShellStream(ctx, script, func(line string) {
		logDiskConversionLine(d.logger, "vm_export_template", line)
	}); err != nil {
		d.logger.Warn("vm_export_template: fixed-to-dynamic disk conversion pass failed", "dir", dir, "error", err)
	}
}
