package download

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlaceFile_CreatesParentDirectoriesAndRenames(t *testing.T) {
	// Create a temp directory to work in
	baseDir := t.TempDir()

	// Create a temp file simulating a downloaded file
	tempFile := filepath.Join(baseDir, "artifact.tmp")
	content := []byte("test artifact content")
	if err := os.WriteFile(tempFile, content, 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	// Destination with nested directories that don't exist yet
	destPath := filepath.Join(baseDir, "nested", "sub", "dir", "artifact.iso")

	// Place the file
	err := PlaceFile(tempFile, destPath)
	if err != nil {
		t.Fatalf("PlaceFile returned unexpected error: %v", err)
	}

	// Verify destination file exists with correct content
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read destination file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("destination content = %q, want %q", got, content)
	}

	// Verify temp file no longer exists
	if _, err := os.Stat(tempFile); !os.IsNotExist(err) {
		t.Errorf("temp file still exists after PlaceFile")
	}
}
