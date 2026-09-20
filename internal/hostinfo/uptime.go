//go:build windows

package hostinfo

import (
	"time"

	"golang.org/x/sys/windows"
)

var (
	modkernel32   = windows.NewLazySystemDLL("kernel32.dll")
	procGetTick64 = modkernel32.NewProc("GetTickCount64")
)

// HostUptime returns the host uptime as days (float64) and the last boot time.
// Uses Windows GetTickCount64 syscall - returns instantly without PowerShell.
func HostUptime() (uptimeDays float64, lastBootTime time.Time) {
	ret, _, _ := procGetTick64.Call()
	tickMs := uint64(ret)

	uptimeDuration := time.Duration(tickMs) * time.Millisecond
	uptimeDays = uptimeDuration.Hours() / 24.0
	lastBootTime = time.Now().Add(-uptimeDuration)
	return uptimeDays, lastBootTime
}
