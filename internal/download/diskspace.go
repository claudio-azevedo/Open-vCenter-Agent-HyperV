//go:build windows

package download

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetVolumeRoot extracts the volume root from a given file path.
// For standard paths like "D:\ISO\file.iso", it returns "D:\".
// For UNC paths or paths where the volume name cannot be determined, it returns an empty string.
func GetVolumeRoot(path string) string {
	vol := filepath.VolumeName(path)
	if vol == "" {
		return ""
	}
	// UNC paths (e.g., \\server\share) have a volume name but we don't support them
	// for disk space checks. filepath.VolumeName returns "\\server\share" for UNC paths.
	if len(vol) > 2 && vol[0] == '\\' && vol[1] == '\\' {
		return ""
	}
	return vol + `\`
}

// CheckDiskSpace returns the number of available bytes on the volume containing
// the given path. It uses the Windows GetDiskFreeSpaceEx API.
// Returns an error if the volume root cannot be determined or the API call fails.
func CheckDiskSpace(path string) (uint64, error) {
	root := GetVolumeRoot(path)
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
