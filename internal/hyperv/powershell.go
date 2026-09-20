package hyperv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// psMutex serializes regular PowerShell executions to avoid concurrency issues with Hyper-V cmdlets.
var psMutex sync.Mutex

// longOperationMutex serializes long-running operations with each other without taking
// psMutex. This lets read-only inventory commands run while operations such as
// Move-VMStorage remain active in their own PowerShell process.
var longOperationMutex sync.Mutex

// psErrorLogPath is the path where PowerShell errors are logged for troubleshooting
var psErrorLogPath string

// psDebugLogPath is the path where all PowerShell executions are logged (only when debug enabled)
var psDebugLogPath string

// psDebugEnabled controls whether debug logging is active
var psDebugEnabled bool

// SetErrorLogDir sets the directory for PowerShell error logs
func SetErrorLogDir(dir string) {
	psErrorLogPath = filepath.Join(dir, "powershell_errors.log")
}

// EnableDebugLog enables debug-level logging of all PowerShell executions.
// When enabled, every script execution (input + output) is written to powershell_debug.log.
func EnableDebugLog(dir string) {
	psDebugLogPath = filepath.Join(dir, "powershell_debug.log")
	psDebugEnabled = true
}

func logPSDebug(script string, stdout string, stderr string, duration time.Duration, err error) {
	if !psDebugEnabled || psDebugLogPath == "" {
		return
	}

	f, ferr := os.OpenFile(psDebugLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if ferr != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format(time.RFC3339)
	status := "OK"
	errMsg := ""
	if err != nil {
		status = "FAILED"
		errMsg = fmt.Sprintf("\nERROR: %v", err)
	}

	entry := fmt.Sprintf(
		"=== %s [%s] duration=%s ===\nSCRIPT:\n%s\nSTDOUT:\n%s\nSTDERR:\n%s%s\n---\n\n",
		timestamp, status, duration.Round(time.Millisecond),
		truncate(script, 2000),
		truncate(stdout, 4000),
		truncate(stderr, 2000),
		errMsg,
	)
	f.WriteString(entry)
}

func logPSError(taskInfo string, script string, stdout string, stderr string, err error) {
	if psErrorLogPath == "" {
		return
	}

	f, ferr := os.OpenFile(psErrorLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if ferr != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format(time.RFC3339)
	entry := fmt.Sprintf(
		"=== %s === [%s]\nERROR: %v\nSTDERR:\n%s\nSTDOUT:\n%s\nSCRIPT (first 500 chars):\n%s\n\n",
		timestamp, taskInfo, err,
		truncate(stderr, 2000),
		truncate(stdout, 2000),
		truncate(script, 500),
	)
	f.WriteString(entry)
}

func truncate(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen] + "...[truncated]"
	}
	return s
}

// controlledErrors are stderr markers that represent intentional validation errors,
// not unexpected failures. These should not be logged to powershell_errors.log.
var controlledErrors = []string{
	"VM_NOT_FOUND",
	"VM_NOT_OFF",
	"SNAPSHOT_NOT_FOUND",
	"ALREADY_HA",
	"NOT_HA",
	"NO_DVD_DRIVE",
	"DVD_ALREADY_MOUNTED",
	"VM_NOT_FOUND_OR_NOT_HA",
}

func isControlledError(stderr string) bool {
	for _, marker := range controlledErrors {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

// RunPowerShell executes a PowerShell command and returns the raw output
func RunPowerShell(ctx context.Context, script string) ([]byte, error) {
	psMutex.Lock()
	defer psMutex.Unlock()

	startTime := time.Now()

	// Prefix script with UTF-8 output encoding to handle accented characters (ç, ã, á, etc.)
	utf8Script := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " + script

	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", utf8Script,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	duration := time.Since(startTime)

	if err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		stdoutStr := strings.TrimSpace(stdout.String())

		// Always log to debug (if enabled)
		logPSDebug(script, stdoutStr, stderrStr, duration, err)

		// Detect context cancellation (timeout)
		if ctx.Err() != nil {
			logPSError("RunPowerShell[TIMEOUT]", script, stdoutStr, stderrStr, ctx.Err())
			return nil, fmt.Errorf("powershell timed out: %v", ctx.Err())
		}

		// Don't log controlled validation errors (agent-generated markers)
		if !isControlledError(stderrStr) {
			logPSError("RunPowerShell", script, stdoutStr, stderrStr, err)
		}

		if stderrStr != "" {
			return nil, fmt.Errorf("powershell error: %s", stderrStr)
		}
		if stdoutStr != "" {
			return nil, fmt.Errorf("powershell execution failed: %s", stdoutStr)
		}
		return nil, fmt.Errorf("powershell execution failed: %v", err)
	}

	// Log successful execution to debug log
	logPSDebug(script, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), duration, nil)

	return stdout.Bytes(), nil
}

// RunPowerShellStream executes a PowerShell command and streams stdout line-by-line
// to the onLine callback as the script produces output. Regular streaming executions
// remain serialized with all other PowerShell commands through psMutex.
func RunPowerShellStream(ctx context.Context, script string, onLine func(line string)) ([]byte, error) {
	psMutex.Lock()
	defer psMutex.Unlock()

	return runPowerShellStream(ctx, script, onLine, "RunPowerShellStream")
}

// RunLongPowerShellStream executes one long-running PowerShell operation in an isolated
// lane. Long operations are serialized with each other but do not hold psMutex, allowing
// periodic read-only inventory commands to run concurrently in separate processes.
func RunLongPowerShellStream(ctx context.Context, script string, onLine func(line string)) ([]byte, error) {
	longOperationMutex.Lock()
	defer longOperationMutex.Unlock()

	return runPowerShellStream(ctx, script, onLine, "RunLongPowerShellStream")
}

// runPowerShellStream contains the shared process and streaming implementation. The
// caller is responsible for acquiring the appropriate execution-lane mutex.
func runPowerShellStream(ctx context.Context, script string, onLine func(line string), runnerName string) ([]byte, error) {
	startTime := time.Now()

	utf8Script := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " + script

	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", utf8Script,
	)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start powershell: %v", err)
	}

	// Read stdout line by line, invoking the callback and collecting the full output
	var stdoutBuf bytes.Buffer
	scanner := bufio.NewScanner(stdoutPipe)
	// Allow long lines (default is 64KB which is usually fine, but be safe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		stdoutBuf.WriteString(line)
		stdoutBuf.WriteString("\n")
		if onLine != nil {
			onLine(strings.TrimSpace(line))
		}
	}

	waitErr := cmd.Wait()
	duration := time.Since(startTime)

	stdoutStr := strings.TrimSpace(stdoutBuf.String())
	stderrStr := strings.TrimSpace(stderr.String())

	if waitErr != nil {
		logPSDebug(script, stdoutStr, stderrStr, duration, waitErr)

		if ctx.Err() != nil {
			logPSError(runnerName+"[TIMEOUT]", script, stdoutStr, stderrStr, ctx.Err())
			return nil, fmt.Errorf("powershell timed out: %v", ctx.Err())
		}

		if !isControlledError(stderrStr) {
			logPSError(runnerName, script, stdoutStr, stderrStr, waitErr)
		}

		if stderrStr != "" {
			return nil, fmt.Errorf("powershell error: %s", stderrStr)
		}
		if stdoutStr != "" {
			return nil, fmt.Errorf("powershell execution failed: %s", stdoutStr)
		}
		return nil, fmt.Errorf("powershell execution failed: %v", waitErr)
	}

	logPSDebug(script, stdoutStr, stderrStr, duration, nil)

	return stdoutBuf.Bytes(), nil
}

// RunPowerShellJSON executes a PowerShell command and parses the JSON output
func RunPowerShellJSON(ctx context.Context, script string, result interface{}) error {
	// Use a ScriptBlock to safely capture output and pipe to ConvertTo-Json
	jsonScript := fmt.Sprintf("& {\n%s\n} | ConvertTo-Json -Depth 10 -Compress", strings.TrimSpace(script))

	output, err := RunPowerShell(ctx, jsonScript)
	if err != nil {
		return err
	}

	// Trim BOM and whitespace
	output = bytes.TrimPrefix(output, []byte("\xef\xbb\xbf"))
	output = bytes.TrimSpace(output)

	if len(output) == 0 {
		return fmt.Errorf("powershell returned empty output")
	}

	if err := json.Unmarshal(output, result); err != nil {
		return fmt.Errorf("failed to parse powershell JSON output: %v (raw: %s)", err, string(output))
	}

	return nil
}

// RunPowerShellRaw executes a PowerShell command and returns raw string output
func RunPowerShellRaw(ctx context.Context, script string) (string, error) {
	output, err := RunPowerShell(ctx, script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
