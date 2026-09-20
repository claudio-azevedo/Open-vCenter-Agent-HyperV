//go:build windows

package hostinfo

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const hashBufferSize = 1 << 20 // 1 MB streaming buffer

// ISOInfo holds information about an ISO file
type ISOInfo struct {
	Path     string `json:"path"`
	Checksum string `json:"checksum"`
}

// ScanISOs lists all .iso files in the given directory and computes MD5 checksums.
// Uses native Go filesystem operations and crypto/md5 - no PowerShell.
// Returns an empty slice (not nil) if the directory doesn't exist or has no ISOs.
func ScanISOs(dirPath string) ([]ISOInfo, error) {
	if dirPath == "" {
		return []ISOInfo{}, nil
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []ISOInfo{}, nil
		}
		return []ISOInfo{}, fmt.Errorf("failed to read ISO directory: %w", err)
	}

	var isos []ISOInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".iso") {
			continue
		}

		fullPath := filepath.Join(dirPath, entry.Name())
		checksum, err := computeMD5(fullPath)
		if err != nil {
			// If we can't hash one file, include it with empty checksum (same behavior as PS version)
			isos = append(isos, ISOInfo{Path: fullPath, Checksum: ""})
			continue
		}
		isos = append(isos, ISOInfo{Path: fullPath, Checksum: checksum})
	}

	if isos == nil {
		isos = []ISOInfo{}
	}
	return isos, nil
}

// computeMD5 calculates the MD5 hash of a file using a 1 MB streaming buffer.
// Returns the uppercase hex-encoded hash (matching PowerShell's Get-FileHash output).
func computeMD5(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := md5.New()
	buf := make([]byte, hashBufferSize)

	if _, err := io.CopyBuffer(hasher, f, buf); err != nil {
		return "", err
	}

	return strings.ToUpper(hex.EncodeToString(hasher.Sum(nil))), nil
}
