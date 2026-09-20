package download

import (
	"fmt"
	"os"
	"path/filepath"
)

// PlaceFile moves the temporary file at tempPath to destinationPath atomically.
// It creates any missing parent directories of destinationPath using os.MkdirAll
// and then renames the temp file to the destination. Both paths must reside on the
// same volume for the rename to succeed as an atomic operation.
//
// If a regular file already exists at destinationPath and allowOverwrite is true,
// it will be removed before the rename. If allowOverwrite is false and a file exists,
// an error is returned. Directories are NEVER removed - if destinationPath points to
// an existing directory, an error is always returned.
//
// All errors are wrapped with fmt.Errorf using %w so callers can inspect the
// underlying OS error with errors.Is and errors.As (e.g., os.ErrPermission,
// os.ErrNotExist). Error messages include the paths involved and the operation
// that failed for easier debugging.
func PlaceFile(tempPath string, destinationPath string) error {
	return PlaceFileWithOverwrite(tempPath, destinationPath, true)
}

// PlaceFileWithOverwrite moves the temporary file at tempPath to destinationPath.
// If allowOverwrite is true and a regular file exists at destination, it is removed
// before the rename. If allowOverwrite is false, existing files cause an error.
// Directories at the destination path ALWAYS cause an error (never removed).
func PlaceFileWithOverwrite(tempPath string, destinationPath string, allowOverwrite bool) error {
	// Create any missing parent directories for the destination path
	parentDir := filepath.Dir(destinationPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directories %q: %w", parentDir, err)
	}

	// Check if destination already exists
	if info, err := os.Stat(destinationPath); err == nil {
		if info.IsDir() {
			return fmt.Errorf("destination path %q is an existing directory, cannot overwrite a directory with a file", destinationPath)
		}
		// It's a regular file
		if !allowOverwrite {
			return fmt.Errorf("destination file %q already exists and overwrite is not allowed", destinationPath)
		}
		// Remove existing file to allow overwrite (required on Windows where
		// os.Rename cannot overwrite an existing file)
		if err := os.Remove(destinationPath); err != nil {
			return fmt.Errorf("failed to remove existing destination file %q: %w", destinationPath, err)
		}
	}

	// Atomically move the temp file to the destination path
	if err := os.Rename(tempPath, destinationPath); err != nil {
		return fmt.Errorf("failed to rename temp file %q to destination %q: %w", tempPath, destinationPath, err)
	}

	return nil
}
