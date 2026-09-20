// Package preflight runs startup checks that must pass before the agent will
// serve requests. The Hyper-V role check is the main one: an ovc-agent-hyperv
// build is useless on a host without Hyper-V.
package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FailureLogName is written next to the agent binary (and log) when a preflight
// check fails, so an operator can see why the service refused to start.
const FailureLogName = "agent_failed.log"

// WriteFailure appends a timestamped failure entry to <dir>/agent_failed.log.
// Best-effort: logging the failure must never itself abort startup handling.
func WriteFailure(dir string, cause error) {
	path := filepath.Join(dir, FailureLogName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\tpreflight failed: %v\n", time.Now().UTC().Format(time.RFC3339), cause)
}

// ClearFailure removes a stale agent_failed.log after a successful startup.
func ClearFailure(dir string) {
	_ = os.Remove(filepath.Join(dir, FailureLogName))
}
