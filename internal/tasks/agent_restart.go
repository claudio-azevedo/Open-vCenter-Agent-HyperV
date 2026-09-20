package tasks

import (
	"context"
	"fmt"
	"os/exec"
)

// handleRestartAgent restarts the ovc-agent Windows service. The restart is
// performed by a detached child process so the agent can reply to the backend
// before its own service stops. A short delay gives the response time to flush.
func (d *Dispatcher) handleRestartAgent(ctx context.Context) (*HostManagementResult, error) {
	// "sc stop" then "sc start", run detached via cmd so it survives our exit.
	cmd := exec.Command("cmd", "/C", "ping -n 3 127.0.0.1 >NUL & sc stop ovc-agent >NUL & sc start ovc-agent >NUL")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to schedule agent restart: %w", err)
	}
	_ = cmd.Process.Release()

	d.logger.Info("host_restart_agent: agent service restart scheduled")
	return &HostManagementResult{
		Hostname: getHostname(ctx),
		Action:   "host_restart_agent",
		Status:   "restarting",
	}, nil
}
