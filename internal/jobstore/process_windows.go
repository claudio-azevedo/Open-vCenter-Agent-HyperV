//go:build windows

package jobstore

import "golang.org/x/sys/windows"

// isProcessAlive checks if a process with the given PID is still running.
// On Windows we open the process handle and read its exit code - STILL_ACTIVE
// (259) means the process is still running.
func isProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == 259
}
