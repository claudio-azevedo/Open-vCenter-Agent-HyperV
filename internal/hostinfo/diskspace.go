//go:build windows

package hostinfo

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DiskSpaceInfo holds total and free space for a volume
type DiskSpaceInfo struct {
	TotalGB float64
	FreeGB  float64
}

// GetDiskSpace returns total and free space in GB for the volume that contains the given path.
// Uses the Windows GetDiskFreeSpaceEx API directly - no PowerShell overhead.
func GetDiskSpace(path string) (*DiskSpaceInfo, error) {
	root := volumeRoot(path)
	if root == "" {
		return nil, fmt.Errorf("cannot determine volume root for path: %s", path)
	}

	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, fmt.Errorf("failed to convert volume root to UTF-16: %w", err)
	}

	var freeBytesAvailable uint64
	var totalBytes uint64

	err = windows.GetDiskFreeSpaceEx(
		rootPtr,
		(*uint64)(unsafe.Pointer(&freeBytesAvailable)),
		(*uint64)(unsafe.Pointer(&totalBytes)),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("GetDiskFreeSpaceEx failed for volume %s: %w", root, err)
	}

	const gb = 1024.0 * 1024.0 * 1024.0
	return &DiskSpaceInfo{
		TotalGB: float64(totalBytes) / gb,
		FreeGB:  float64(freeBytesAvailable) / gb,
	}, nil
}

// GetDiskSpaceFreeBytes returns available free bytes for the volume containing path.
// Convenience function for disk space checks that work with raw bytes.
func GetDiskSpaceFreeBytes(path string) (uint64, error) {
	root := volumeRoot(path)
	if root == "" {
		return 0, fmt.Errorf("cannot determine volume root for path: %s", path)
	}

	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, fmt.Errorf("failed to convert volume root to UTF-16: %w", err)
	}

	var freeBytesAvailable uint64
	err = windows.GetDiskFreeSpaceEx(
		rootPtr,
		(*uint64)(unsafe.Pointer(&freeBytesAvailable)),
		nil,
		nil,
	)
	if err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx failed for volume %s: %w", root, err)
	}

	return freeBytesAvailable, nil
}

// volumeRoot extracts the volume root (e.g. "C:\") from a path.
// Returns empty string for UNC paths or paths without a volume name.
func volumeRoot(path string) string {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return ""
	}
	// UNC paths not supported
	if len(vol) > 2 && vol[0] == '\\' && vol[1] == '\\' {
		return ""
	}
	return vol + `\`
}
