package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyChecksum_MatchLowercase(t *testing.T) {
	// Create a temp file with known content
	content := []byte("hello world")
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Compute expected hash
	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	match, computed, err := VerifyChecksum(filePath, expected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Errorf("expected match, got mismatch: computed=%s expected=%s", computed, expected)
	}
	if computed != expected {
		t.Errorf("computed hash %q != expected %q", computed, expected)
	}
}

func TestVerifyChecksum_MatchUppercaseExpected(t *testing.T) {
	// Verifies case-insensitive comparison: expected is UPPERCASE, computed is lowercase
	content := []byte("case insensitive test")
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	h := sha256.Sum256(content)
	expectedUpper := strings.ToUpper(hex.EncodeToString(h[:]))

	match, computed, err := VerifyChecksum(filePath, expectedUpper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Errorf("expected case-insensitive match, got mismatch: computed=%s expected=%s", computed, expectedUpper)
	}
	// computed should always be lowercase
	if computed != strings.ToLower(computed) {
		t.Errorf("computed hash should be lowercase, got %q", computed)
	}
}

func TestVerifyChecksum_MatchMixedCaseExpected(t *testing.T) {
	// Verifies case-insensitive comparison: expected has mixed case
	content := []byte("mixed case check")
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	h := sha256.Sum256(content)
	hashStr := hex.EncodeToString(h[:])
	// Create a mixed-case version: alternate upper/lower
	mixedCase := make([]byte, len(hashStr))
	for i, c := range []byte(hashStr) {
		if i%2 == 0 {
			mixedCase[i] = c
		} else {
			mixedCase[i] = []byte(strings.ToUpper(string(c)))[0]
		}
	}

	match, computed, err := VerifyChecksum(filePath, string(mixedCase))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Errorf("expected case-insensitive match, got mismatch: computed=%s expected=%s", computed, string(mixedCase))
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	// Verifies that mismatch returns false and provides computed hash for error message
	content := []byte("actual content")
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	wrongExpected := "0000000000000000000000000000000000000000000000000000000000000000"

	match, computed, err := VerifyChecksum(filePath, wrongExpected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match {
		t.Error("expected mismatch, got match")
	}
	// Verify computed hash is returned and is lowercase hex
	if computed == "" {
		t.Error("computed hash should not be empty on mismatch")
	}
	if computed != strings.ToLower(computed) {
		t.Errorf("computed hash should be lowercase, got %q", computed)
	}
	if len(computed) != 64 {
		t.Errorf("computed hash should be 64 hex chars, got %d chars: %q", len(computed), computed)
	}
	// Verify it's the actual SHA-256 of the content
	h := sha256.Sum256(content)
	actualHash := hex.EncodeToString(h[:])
	if computed != actualHash {
		t.Errorf("computed hash %q doesn't match actual SHA-256 %q", computed, actualHash)
	}
}

func TestVerifyChecksum_FileNotFound(t *testing.T) {
	_, _, err := VerifyChecksum("/nonexistent/path/file.bin", "abcd1234")
	if err == nil {
		t.Error("expected error for non-existent file, got nil")
	}
}

func TestVerifyChecksum_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "empty.bin")
	if err := os.WriteFile(filePath, []byte{}, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// SHA-256 of empty data
	h := sha256.Sum256([]byte{})
	expected := hex.EncodeToString(h[:])

	match, computed, err := VerifyChecksum(filePath, expected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Errorf("expected match for empty file, got mismatch: computed=%s expected=%s", computed, expected)
	}
}

func TestVerifyChecksum_ComputedHashAlwaysLowercase(t *testing.T) {
	// Regardless of expected casing, the returned computed hash must be lowercase
	content := []byte("verify lowercase output")
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Pass uppercase expected
	h := sha256.Sum256(content)
	expectedUpper := strings.ToUpper(hex.EncodeToString(h[:]))

	_, computed, err := VerifyChecksum(filePath, expectedUpper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if computed != strings.ToLower(computed) {
		t.Errorf("computed hash must always be lowercase, got %q", computed)
	}
}

// Feature: artifact-repository-agent, Property 3: Checksum Integrity
// Validates: Requirements 5.1, 5.2, 5.3

func TestVerifyChecksum_KnownChecksumValues(t *testing.T) {
	// Test with pre-computed, hardcoded SHA-256 values to verify correctness
	// of the implementation against known reference values.
	tests := []struct {
		name     string
		content  []byte
		expected string // pre-computed SHA-256 hex
	}{
		{
			name:     "empty content",
			content:  []byte{},
			expected: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:     "hello world",
			content:  []byte("hello world"),
			expected: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		},
		{
			name:     "single null byte",
			content:  []byte{0x00},
			expected: "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
		},
		{
			name:     "single newline",
			content:  []byte("\n"),
			expected: "01ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "test.bin")
			if err := os.WriteFile(filePath, tc.content, 0644); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}

			match, computed, err := VerifyChecksum(filePath, tc.expected)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !match {
				t.Errorf("expected match for %q: computed=%s expected=%s", tc.name, computed, tc.expected)
			}
			if computed != tc.expected {
				t.Errorf("computed hash %q != expected %q", computed, tc.expected)
			}
		})
	}
}

func TestVerifyChecksum_LargeFileExceedsBufferSize(t *testing.T) {
	// Create a file larger than the 1 MB buffer (checksumBufferSize) to verify
	// that chunked reading across multiple buffer fills produces a correct hash.
	// Using 2.5 MB to cross the buffer boundary multiple times.
	size := 2*1024*1024 + 512*1024 // 2.5 MB
	content := bytes.Repeat([]byte("ABCDEFGHIJ"), size/10)

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "large.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Compute expected hash independently
	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	match, computed, err := VerifyChecksum(filePath, expected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Errorf("expected match for large file, got mismatch: computed=%s expected=%s", computed, expected)
	}
	if computed != expected {
		t.Errorf("computed hash %q != expected %q for large file", computed, expected)
	}
}

func TestVerifyChecksum_ExactBufferBoundary(t *testing.T) {
	// File size exactly equal to the 1 MB buffer to test boundary condition
	size := 1024 * 1024 // exactly 1 MB = checksumBufferSize
	content := bytes.Repeat([]byte{0xAB}, size)

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "exact_buffer.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	match, computed, err := VerifyChecksum(filePath, expected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Errorf("expected match for exact-buffer-size file, got mismatch: computed=%s expected=%s", computed, expected)
	}
}

func TestVerifyChecksum_BufferBoundaryPlusOne(t *testing.T) {
	// File size is buffer + 1 byte to test the boundary where a second read is needed for just 1 byte
	size := 1024*1024 + 1
	content := bytes.Repeat([]byte{0xCD}, size)

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "buffer_plus_one.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	match, computed, err := VerifyChecksum(filePath, expected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Errorf("expected match for buffer+1 file, got mismatch: computed=%s expected=%s", computed, expected)
	}
}

func TestVerifyChecksum_IntegrityProperty(t *testing.T) {
	// Property 3: Checksum Integrity - when VerifyChecksum returns match=true,
	// the file content on disk has not been corrupted, proven by independently
	// re-reading and hashing the file.
	content := []byte("integrity verification content for property 3")
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "integrity.bin")
	if err := os.WriteFile(filePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	h := sha256.Sum256(content)
	expected := hex.EncodeToString(h[:])

	match, computed, err := VerifyChecksum(filePath, expected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !match {
		t.Fatalf("expected match, got mismatch")
	}

	// Independently verify: re-read the file and confirm the content is intact
	readBack, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to re-read file: %v", err)
	}

	recomputedHash := sha256.Sum256(readBack)
	recomputedHex := hex.EncodeToString(recomputedHash[:])

	if recomputedHex != computed {
		t.Errorf("integrity violated: re-read hash %q != computed hash %q", recomputedHex, computed)
	}
	if !strings.EqualFold(recomputedHex, expected) {
		t.Errorf("integrity violated: re-read hash %q != expected payload checksum %q", recomputedHex, expected)
	}
}
