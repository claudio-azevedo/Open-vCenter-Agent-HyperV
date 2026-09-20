package upgrade

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	ServiceName    = "ovc-agent"
	BinaryName     = "ovc-agent.exe"
	BackupSuffix   = ".bak"
	PendingFile    = "upgrade-pending.json"
	StopTimeout    = 90 * time.Second
	StartupTimeout = 30 * time.Second
	PollInterval   = 2 * time.Second
)

// PendingUpgradeResponse holds the information needed to publish a completion response
// after the new agent starts. Written to disk before upgrade, read on startup.
type PendingUpgradeResponse struct {
	TaskID     string `json:"task_id"`
	Version    string `json:"version"`
	SizeBytes  int64  `json:"size_bytes"`
	OldVersion string `json:"old_version"`
	NewVersion string `json:"new_version"`
}

// Run executes the full upgrade procedure. It is called when the agent binary
// is invoked with the "upgrade" argument (e.g., ovc-agent-{uuid}.exe upgrade).
//
// Steps:
//  1. Validate service is running
//  2. Validate directory is writable
//  3. Stop the service via SCM
//  4. Poll until service is stopped (timeout 90s)
//  5. Rename ovc-agent.exe → ovc-agent.exe.bak
//  6. Rename self (ovc-agent-{uuid}.exe) → ovc-agent.exe
//  7. Start service via SCM
//  8. Poll log for startup confirmation (timeout 30s)
//  9. On success → log, exit 0
//  10. On failure → rollback, restart, log error, exit 1
func Run(logger *slog.Logger) error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to determine own executable path: %w", err)
	}
	selfPath, err = filepath.EvalSymlinks(selfPath)
	if err != nil {
		return fmt.Errorf("failed to resolve own executable path: %w", err)
	}

	dir := filepath.Dir(selfPath)
	targetPath := filepath.Join(dir, BinaryName)
	backupPath := targetPath + BackupSuffix

	logger.Info("upgrade: starting",
		"self", selfPath,
		"target", targetPath,
		"backup", backupPath,
	)

	// Step 1: Validate service is running
	logger.Info("upgrade: step 1 - validating service is running")
	if err := validateServiceRunning(); err != nil {
		return fmt.Errorf("step 1 failed: %w", err)
	}

	// Step 2: Validate directory is writable
	logger.Info("upgrade: step 2 - validating directory is writable")
	if err := validateDirWritable(dir); err != nil {
		return fmt.Errorf("step 2 failed: %w", err)
	}

	// Step 3: Stop the service
	logger.Info("upgrade: step 3 - stopping service")
	if err := stopService(); err != nil {
		return fmt.Errorf("step 3 failed: %w", err)
	}

	// Step 4: Poll until stopped
	logger.Info("upgrade: step 4 - waiting for service to stop")
	if err := waitForServiceStopped(StopTimeout); err != nil {
		return fmt.Errorf("step 4 failed: %w", err)
	}
	logger.Info("upgrade: service stopped successfully")

	// Step 5: Backup current binary
	logger.Info("upgrade: step 5 - backing up current binary")
	// Remove old backup if it exists
	if _, err := os.Stat(backupPath); err == nil {
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("step 5 failed: cannot remove old backup: %w", err)
		}
	}
	if err := os.Rename(targetPath, backupPath); err != nil {
		return fmt.Errorf("step 5 failed: cannot backup current binary: %w", err)
	}

	// Step 6: Rename self to target
	logger.Info("upgrade: step 6 - renaming new binary to target")
	if err := os.Rename(selfPath, targetPath); err != nil {
		// Rollback: restore backup
		logger.Error("upgrade: step 6 failed, rolling back", "error", err)
		os.Rename(backupPath, targetPath)
		invalidatePendingResponse(dir, logger)
		return fmt.Errorf("step 6 failed: cannot rename new binary: %w", err)
	}

	// Step 7: Start service
	logger.Info("upgrade: step 7 - starting service")
	if err := startService(); err != nil {
		// Rollback: restore backup and restart
		logger.Error("upgrade: step 7 failed, rolling back", "error", err)
		os.Rename(targetPath, selfPath) // move new binary back
		os.Rename(backupPath, targetPath)
		startService() // best effort restart with old binary
		invalidatePendingResponse(dir, logger)
		return fmt.Errorf("step 7 failed: cannot start service: %w", err)
	}

	// Step 8: Poll log for startup confirmation
	logger.Info("upgrade: step 8 - verifying new agent started")
	logFile := filepath.Join(dir, "agent.log")
	if err := waitForStartupLog(logFile, StartupTimeout); err != nil {
		// Rollback: stop new, restore old, restart
		logger.Error("upgrade: step 8 failed, rolling back", "error", err)
		stopService()
		waitForServiceStopped(30 * time.Second)
		os.Rename(targetPath, targetPath+".failed")
		os.Rename(backupPath, targetPath)
		startService()
		invalidatePendingResponse(dir, logger)
		return fmt.Errorf("step 8 failed: new agent did not start correctly: %w", err)
	}

	logger.Info("upgrade: completed successfully")
	return nil
}

// invalidatePendingResponse removes the pending upgrade response file after a failed
// upgrade + rollback. This is critical: without it, the rolled-back OLD agent would
// read the leftover pending file on startup and publish a false "upgraded: true"
// completion, reporting success for an upgrade that actually failed.
func invalidatePendingResponse(dir string, logger *slog.Logger) {
	path := filepath.Join(dir, PendingFile)
	if _, err := os.Stat(path); err != nil {
		return // nothing to remove
	}
	if err := os.Remove(path); err != nil {
		logger.Warn("upgrade: failed to remove stale pending response after rollback",
			"path", path, "error", err)
		return
	}
	logger.Info("upgrade: removed stale pending response after rollback", "path", path)
}

// validateServiceRunning checks that the ovc-agent service exists and is running.
func validateServiceRunning() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s not found: %w", ServiceName, err)
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("cannot query service status: %w", err)
	}

	if status.State != svc.Running {
		return fmt.Errorf("service is not running (current state: %d)", status.State)
	}

	return nil
}

// validateDirWritable confirms the directory is writable by creating and removing a temp file.
func validateDirWritable(dir string) error {
	testFile := filepath.Join(dir, ".upgrade-write-test")
	f, err := os.Create(testFile)
	if err != nil {
		return fmt.Errorf("directory %q is not writable: %w", dir, err)
	}
	f.Close()
	os.Remove(testFile)
	return nil
}

// stopService sends a Stop control command to the service via SCM.
func stopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s not found: %w", ServiceName, err)
	}
	defer s.Close()

	_, err = s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("failed to send stop command: %w", err)
	}

	return nil
}

// startService starts the service via SCM.
func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s not found: %w", ServiceName, err)
	}
	defer s.Close()

	return s.Start("run")
}

// waitForServiceStopped polls the service status until it's stopped or timeout expires.
func waitForServiceStopped(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		m, err := mgr.Connect()
		if err != nil {
			time.Sleep(PollInterval)
			continue
		}

		s, err := m.OpenService(ServiceName)
		if err != nil {
			m.Disconnect()
			time.Sleep(PollInterval)
			continue
		}

		status, err := s.Query()
		s.Close()
		m.Disconnect()

		if err != nil {
			time.Sleep(PollInterval)
			continue
		}

		if status.State == svc.Stopped {
			return nil
		}

		time.Sleep(PollInterval)
	}

	return fmt.Errorf("service did not stop within %v", timeout)
}

// waitForStartupLog polls the agent.log file looking for a "starting ovc-agent"
// message with a timestamp newer than the current time (i.e., the new instance starting).
func waitForStartupLog(logFile string, timeout time.Duration) error {
	startTime := time.Now()
	deadline := startTime.Add(timeout)

	for time.Now().Before(deadline) {
		if found, _ := checkLogForStartup(logFile, startTime); found {
			return nil
		}
		time.Sleep(PollInterval)
	}

	return fmt.Errorf("no startup confirmation found in %s within %v", logFile, timeout)
}

// checkLogForStartup reads the last 50 lines of the log file and looks for
// a "starting ovc-agent" message with a timestamp after startTime.
func checkLogForStartup(logFile string, startTime time.Time) (bool, error) {
	f, err := os.Open(logFile)
	if err != nil {
		return false, err
	}
	defer f.Close()

	// Read last 8KB of the file (should contain recent log entries)
	const tailSize = 8192
	info, err := f.Stat()
	if err != nil {
		return false, err
	}

	offset := int64(0)
	if info.Size() > tailSize {
		offset = info.Size() - tailSize
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// The JSON log entry contains "msg":"starting ovc-agent"
		if strings.Contains(line, "starting ovc-agent") {
			// Check if timestamp in the line is after our startTime
			// JSON log format: {"time":"2026-07-27T10:00:00Z", ...}
			if ts := extractTimestamp(line); !ts.IsZero() {
				if ts.After(startTime) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// extractTimestamp extracts the "time" field from a JSON log line.
func extractTimestamp(line string) time.Time {
	// Look for "time":"..." pattern in the JSON line
	const key = `"time":"`
	idx := strings.Index(line, key)
	if idx < 0 {
		return time.Time{}
	}

	start := idx + len(key)
	end := strings.Index(line[start:], `"`)
	if end < 0 {
		return time.Time{}
	}

	ts, err := time.Parse(time.RFC3339, line[start:start+end])
	if err != nil {
		// Try RFC3339Nano
		ts, err = time.Parse(time.RFC3339Nano, line[start:start+end])
		if err != nil {
			return time.Time{}
		}
	}

	return ts
}

// WritePendingResponse saves upgrade task metadata to disk so the new agent can
// publish the final "completed" response after a successful upgrade.
func WritePendingResponse(dir string, pending *PendingUpgradeResponse) error {
	data, err := json.Marshal(pending)
	if err != nil {
		return fmt.Errorf("failed to marshal pending response: %w", err)
	}

	path := filepath.Join(dir, PendingFile)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write pending response file: %w", err)
	}

	return nil
}

// ReadPendingResponse reads and removes the pending upgrade response file.
// Returns nil if no pending response exists.
func ReadPendingResponse(dir string) *PendingUpgradeResponse {
	path := filepath.Join(dir, PendingFile)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil // file doesn't exist or can't be read - no pending response
	}

	// Remove the file immediately so it's not processed again on next restart
	os.Remove(path)

	var pending PendingUpgradeResponse
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil
	}

	return &pending
}

// PeekPendingResponse reads the pending upgrade response file WITHOUT removing it.
// It is used by the upgrade process to recover the task_id so it can publish a
// failure response when the upgrade fails. Returns nil if none exists.
func PeekPendingResponse(dir string) *PendingUpgradeResponse {
	path := filepath.Join(dir, PendingFile)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var pending PendingUpgradeResponse
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil
	}

	return &pending
}

// GetAgentDir returns the directory where the agent binary is located.
func GetAgentDir() string {
	exePath, err := os.Executable()
	if err != nil {
		return ""
	}
	exePath, _ = filepath.EvalSymlinks(exePath)
	return filepath.Dir(exePath)
}

// Cleanup removes any leftover .bak files and uuid-named binaries from the agent directory.
// Called by the agent on normal startup to clean up after a successful upgrade.
func Cleanup(logger *slog.Logger) {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return
	}

	dir := filepath.Dir(exePath)

	// Remove .bak file
	backupPath := filepath.Join(dir, BinaryName+BackupSuffix)
	if _, err := os.Stat(backupPath); err == nil {
		if err := os.Remove(backupPath); err != nil {
			logger.Warn("upgrade cleanup: failed to remove backup", "path", backupPath, "error", err)
		} else {
			logger.Info("upgrade cleanup: removed backup", "path", backupPath)
		}
	}

	// Remove any .failed file
	failedPath := filepath.Join(dir, BinaryName+".failed")
	if _, err := os.Stat(failedPath); err == nil {
		if err := os.Remove(failedPath); err != nil {
			logger.Warn("upgrade cleanup: failed to remove failed binary", "path", failedPath, "error", err)
		} else {
			logger.Info("upgrade cleanup: removed failed binary", "path", failedPath)
		}
	}

	// Remove any ovc-agent-*.exe files (leftover uuid binaries)
	matches, err := filepath.Glob(filepath.Join(dir, "ovc-agent-*.exe"))
	if err != nil {
		return
	}
	for _, match := range matches {
		// Skip the main binary itself
		if filepath.Base(match) == BinaryName {
			continue
		}
		if err := os.Remove(match); err != nil {
			logger.Warn("upgrade cleanup: failed to remove leftover binary", "path", match, "error", err)
		} else {
			logger.Info("upgrade cleanup: removed leftover binary", "path", match)
		}
	}
}
