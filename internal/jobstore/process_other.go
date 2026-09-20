//go:build !windows

package jobstore

import (
	"os"
	"syscall"
)

// isProcessAlive checks if a process with the given PID is still running.
// Non-Windows implementation (used for local builds/tests): signal 0 probes
// the process without affecting it.
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
