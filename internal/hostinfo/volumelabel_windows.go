//go:build windows

package hostinfo

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// GetVolumeLabel returns the Windows volume label for the volume that contains
// the given path (e.g. "DADOS" for "E:\HyperV"). Returns an empty string if the
// volume has no label or the label cannot be read.
func GetVolumeLabel(path string) string {
	root := volumeRoot(path)
	if root == "" {
		return ""
	}

	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return ""
	}

	volNameBuf := make([]uint16, windows.MAX_PATH+1)

	err = windows.GetVolumeInformation(
		rootPtr,
		&volNameBuf[0],
		uint32(len(volNameBuf)),
		nil, // serial number
		nil, // max component length
		nil, // file system flags
		nil, // file system name buffer
		0,   // file system name size
	)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(windows.UTF16ToString(volNameBuf))
}

// VolumeDisplayName returns the volume label for the path, falling back to the
// drive letter (e.g. "E:") when the volume has no label.
func VolumeDisplayName(path string) string {
	if label := GetVolumeLabel(path); label != "" {
		return label
	}
	return strings.ToUpper(filepath.VolumeName(path))
}
