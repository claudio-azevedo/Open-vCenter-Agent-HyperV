package tasks

import (
	"context"
	"fmt"
	"strings"

	"ovc-agent/internal/hyperv"
)

// resolveVMName looks up a VM's name from its Hyper-V Id. Used because the
// backend addresses VMs by vm_id while most handlers work with the name.
func (d *Dispatcher) resolveVMName(ctx context.Context, vmID string) (string, error) {
	out, err := hyperv.RunPowerShellRaw(ctx,
		fmt.Sprintf(`(Get-VM -Id '%s' -ErrorAction Stop).Name`, escapePSSingleQuoted(vmID)))
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("no VM found with id %s", vmID)
	}
	return name, nil
}

// shutdownGraceSeconds is how long the shutdown script waits for the guest to
// power off. Kept comfortably under the vm_shutdown task timeout (dispatcher.go
// 9m taskCtx / jobqueue.Monitor 600s process-kill).
const shutdownGraceSeconds = 480

// handleVMShutdownAction performs a graceful guest-OS shutdown.
//
// `Stop-VM` runs plain `-AsJob` (no `-Force`, so a VM with working Integration
// Services still gets a clean guest shutdown, not a hard stop). `-AsJob` keeps
// our polling loop responsive, which covers both slow cases:
//   - a graceful shutdown that outlasts the old 120s wait - we keep polling;
//   - a guest that can't be shut down (no / unresponsive Integration Services,
//     which would otherwise raise a confirmation Hyper-V can't answer in a
//     background job) - the loop's own deadline fires and we report a failure
//     instead of the worker being killed on timeout.
//
// The VM is NOT force-turned-off: if the guest never powers down the task fails
// and the caller can fall back to Turn Off.
func (d *Dispatcher) handleVMShutdownAction(ctx context.Context, p VMManagementPayload) (*VMManagementResult, error) {
	if p.VMName == "" {
		return nil, fmt.Errorf("vm_name is required for vm_shutdown action")
	}

	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$name = "%[1]s"
if ((Get-VM -Name $name).State -eq 'Off') { 'Off'; return }

$job = Stop-VM -Name $name -AsJob

$deadline = (Get-Date).AddSeconds(%[2]d)
$state = 'Running'
while ((Get-Date) -lt $deadline) {
    $state = (Get-VM -Name $name -ErrorAction SilentlyContinue).State.ToString()
    if ($state -eq 'Off') { break }
    if ($job.State -eq 'Failed') {
        $reason = ''
        try { $reason = "$($job.ChildJobs[0].JobStateInfo.Reason.Message)" } catch {}
        Remove-Job $job -Force -ErrorAction SilentlyContinue
        throw "SHUTDOWN_FAILED: $reason"
    }
    Start-Sleep -Seconds 3
}
Remove-Job $job -Force -ErrorAction SilentlyContinue

if ($state -ne 'Off') {
    throw "SHUTDOWN_FAILED: the guest did not power off within %[2]d seconds - it may have no shutdown integration service. Use Turn Off."
}
$state
`, p.VMName, shutdownGraceSeconds)

	state, err := hyperv.RunPowerShellRaw(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("failed to shut down VM '%s': %s",
			p.VMName, extractPSErrorDetail(err, "SHUTDOWN_FAILED:"))
	}
	state = strings.TrimSpace(state)
	if state == "" {
		state = "Off"
	}
	return &VMManagementResult{
		VMID:   p.VMID,
		VMName: p.VMName,
		Action: "vm_shutdown",
		Status: state,
	}, nil
}
