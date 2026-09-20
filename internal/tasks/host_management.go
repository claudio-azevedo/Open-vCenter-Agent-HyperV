package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ovc-agent/internal/hyperv"
)

// HostManagementPayload is the generic payload for host_management tasks
type HostManagementPayload struct {
	Action string `json:"action"`
}

// HostManagementResult holds the generic result for host_management tasks
type HostManagementResult struct {
	Hostname string `json:"hostname"`
	Action   string `json:"action"`
	Status   string `json:"status"`
}

func (d *Dispatcher) handleHostManagement(ctx context.Context, payload json.RawMessage) (interface{}, error) {
	if payload == nil || string(payload) == "null" || string(payload) == "{}" {
		return nil, fmt.Errorf("payload is required for host_management")
	}

	var p HostManagementPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("invalid payload: %v", err)
	}

	if p.Action == "" {
		return nil, fmt.Errorf("action is required for host_management")
	}

	switch p.Action {
	case "suspend":
		return d.handleHostSuspend(ctx)
	case "suspend_drain":
		return d.handleHostSuspendDrain(ctx)
	case "resume":
		return d.handleHostResume(ctx)
	case "resume_fallback":
		return d.handleHostResumeFallback(ctx)
	case "restart":
		return d.handleHostRestart(ctx)
	case "host_restart_agent":
		return d.handleRestartAgent(ctx)
	case "refresh_hardware":
		return d.handleRefreshHardware(ctx)
	case "refresh_inventory":
		return d.handleRefreshInventory(ctx)
	default:
		return nil, fmt.Errorf("unknown host_management action: %s", p.Action)
	}
}

// handleHostSuspend puts the cluster node in maintenance mode (paused) without draining resources
func (d *Dispatcher) handleHostSuspend(ctx context.Context) (*HostManagementResult, error) {
	if !d.cluster.IsClusterNode {
		return nil, fmt.Errorf("action 'suspend' requires the host to be part of a cluster")
	}

	script := `
$nodeName = $env:COMPUTERNAME
Suspend-ClusterNode -Name $nodeName -ErrorAction Stop | Out-Null
$node = Get-ClusterNode -Name $nodeName -ErrorAction Stop
$node.State.ToString()
`
	output, err := hyperv.RunPowerShellRaw(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("failed to suspend cluster node: %s", extractPSErrorDetail(err))
	}

	return &HostManagementResult{
		Hostname: getHostname(ctx),
		Action:   "suspend",
		Status:   strings.TrimSpace(output),
	}, nil
}

// handleHostSuspendDrain puts the cluster node in maintenance mode, draining (moving) all resources to other nodes
func (d *Dispatcher) handleHostSuspendDrain(ctx context.Context) (*HostManagementResult, error) {
	if !d.cluster.IsClusterNode {
		return nil, fmt.Errorf("action 'suspend_drain' requires the host to be part of a cluster")
	}

	// Use a longer timeout since draining may take a while
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer drainCancel()

	script := `
$nodeName = $env:COMPUTERNAME
Suspend-ClusterNode -Name $nodeName -Drain -ErrorAction Stop | Out-Null
$node = Get-ClusterNode -Name $nodeName -ErrorAction Stop
$node.State.ToString()
`
	output, err := hyperv.RunPowerShellRaw(drainCtx, script)
	if err != nil {
		return nil, fmt.Errorf("failed to suspend cluster node with drain: %s", extractPSErrorDetail(err))
	}

	return &HostManagementResult{
		Hostname: getHostname(ctx),
		Action:   "suspend_drain",
		Status:   strings.TrimSpace(output),
	}, nil
}

// handleHostResume takes the cluster node out of maintenance mode without failing back resources
func (d *Dispatcher) handleHostResume(ctx context.Context) (*HostManagementResult, error) {
	if !d.cluster.IsClusterNode {
		return nil, fmt.Errorf("action 'resume' requires the host to be part of a cluster")
	}

	script := `
$nodeName = $env:COMPUTERNAME
Resume-ClusterNode -Name $nodeName -ErrorAction Stop | Out-Null
$node = Get-ClusterNode -Name $nodeName -ErrorAction Stop
$node.State.ToString()
`
	output, err := hyperv.RunPowerShellRaw(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("failed to resume cluster node: %s", extractPSErrorDetail(err))
	}

	return &HostManagementResult{
		Hostname: getHostname(ctx),
		Action:   "resume",
		Status:   strings.TrimSpace(output),
	}, nil
}

// handleHostResumeFallback takes the cluster node out of maintenance mode and fails back resources immediately
func (d *Dispatcher) handleHostResumeFallback(ctx context.Context) (*HostManagementResult, error) {
	if !d.cluster.IsClusterNode {
		return nil, fmt.Errorf("action 'resume_fallback' requires the host to be part of a cluster")
	}

	script := `
$nodeName = $env:COMPUTERNAME
Resume-ClusterNode -Name $nodeName -Failback Immediate -ErrorAction Stop | Out-Null
$node = Get-ClusterNode -Name $nodeName -ErrorAction Stop
$node.State.ToString()
`
	output, err := hyperv.RunPowerShellRaw(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("failed to resume cluster node with failback: %s", extractPSErrorDetail(err))
	}

	return &HostManagementResult{
		Hostname: getHostname(ctx),
		Action:   "resume_fallback",
		Status:   strings.TrimSpace(output),
	}, nil
}

// handleHostRestart restarts the host.
// Pre-checks: if cluster, node must be Suspended (Paused). No running VMs allowed on this host.
func (d *Dispatcher) handleHostRestart(ctx context.Context) (*HostManagementResult, error) {
	// Pre-check: if cluster node, verify it is in Paused/Suspended state
	if d.cluster.IsClusterNode {
		stateScript := `
$nodeName = $env:COMPUTERNAME
$node = Get-ClusterNode -Name $nodeName -ErrorAction Stop
$node.State.ToString()
`
		stateOutput, err := hyperv.RunPowerShellRaw(ctx, stateScript)
		if err != nil {
			return nil, fmt.Errorf("failed to check cluster node state: %s", extractPSErrorDetail(err))
		}
		nodeState := strings.TrimSpace(stateOutput)
		if nodeState != "Paused" {
			return nil, fmt.Errorf("cluster node must be in maintenance mode (Paused) before restart, current state: %s", nodeState)
		}
	}

	// Pre-check: verify no running VMs on this host
	runningScript := `
$running = Get-VM | Where-Object { $_.State -eq 'Running' }
if ($running) { $running.Count } else { 0 }
`
	runningOutput, err := hyperv.RunPowerShellRaw(ctx, runningScript)
	if err != nil {
		return nil, fmt.Errorf("failed to check running VMs: %s", extractPSErrorDetail(err))
	}
	runningCount := strings.TrimSpace(runningOutput)
	if runningCount != "0" {
		return nil, fmt.Errorf("cannot restart host: there are %s running VMs on this host", runningCount)
	}

	// All pre-checks passed, issue the restart
	script := `Restart-Computer -Force`
	_, err = hyperv.RunPowerShell(ctx, script)
	if err != nil {
		// Restart-Computer may not return cleanly since the machine is shutting down
		// If the error indicates the connection was lost, that's expected
		if !strings.Contains(err.Error(), "broken pipe") &&
			!strings.Contains(err.Error(), "connection reset") &&
			!strings.Contains(err.Error(), "exit status") {
			return nil, fmt.Errorf("failed to restart host: %v", err)
		}
	}

	return &HostManagementResult{
		Hostname: getHostname(ctx),
		Action:   "restart",
		Status:   "restarting",
	}, nil
}

// getHostname retrieves the local hostname via PowerShell
func getHostname(ctx context.Context) string {
	output, err := hyperv.RunPowerShellRaw(ctx, `$env:COMPUTERNAME`)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(output)
}

// handleRefreshHardware triggers a hardware inventory refresh and publishes to state queue
func (d *Dispatcher) handleRefreshHardware(ctx context.Context) (interface{}, error) {
	result, err := d.handleHardwareInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh hardware inventory: %v", err)
	}

	// Publish to hardware_inventory state queue asynchronously
	if d.onHardwareRefresh != nil {
		go func() {
			refreshResponse := &TaskResponse{
				TaskID:    fmt.Sprintf("refresh-hardware-%d", time.Now().Unix()),
				HostID:    d.hostID,
				Type:      TaskHardwareInventory,
				Status:    "completed",
				Result:    result,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			}
			d.onHardwareRefresh(ctx, refreshResponse)
		}()
	}

	return result, nil
}

// handleRefreshInventory triggers a VM inventory refresh and publishes to state queue
func (d *Dispatcher) handleRefreshInventory(ctx context.Context) (interface{}, error) {
	result, err := d.handleVMInventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh VM inventory: %v", err)
	}

	// Publish to vm_inventory state queue asynchronously
	if d.onVMInventoryRefresh != nil {
		go func() {
			refreshResponse := &TaskResponse{
				TaskID:    fmt.Sprintf("refresh-vm-inventory-%d", time.Now().Unix()),
				HostID:    d.hostID,
				Type:      TaskVMInventory,
				Status:    "completed",
				Result:    result,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			}
			d.onVMInventoryRefresh(ctx, refreshResponse)
		}()
	}

	return result, nil
}
