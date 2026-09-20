package download

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

const checksumBufferSize = 1 << 20 // 1 MB

// VerifyChecksum computes the SHA-256 hash of the file at filePath using a 1 MB buffer
// and compares it against expectedSHA256 (case-insensitive).
// Returns:
//   - match: true if the computed hash matches the expected hash
//   - computedHash: the lowercase hex-encoded SHA-256 of the file
//   - err: any I/O error encountered while reading the file
func VerifyChecksum(filePath string, expectedSHA256 string) (bool, string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return false, "", fmt.Errorf("failed to open file for checksum verification: %w", err)
	}
	defer f.Close()

	hasher := sha256.New()
	buf := make([]byte, checksumBufferSize)

	if _, err := io.CopyBuffer(hasher, f, buf); err != nil {
		return false, "", fmt.Errorf("failed to read file for checksum computation: %w", err)
	}

	computedHash := hex.EncodeToString(hasher.Sum(nil))
	match := strings.EqualFold(computedHash, expectedSHA256)

	return match, computedHash, nil
}
