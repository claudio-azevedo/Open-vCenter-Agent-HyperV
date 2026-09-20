//go:build windows

package download

import "testing"

func TestGetVolumeRoot(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "standard path with subdirectories",
			path:     `D:\ISO\file.iso`,
			expected: `D:\`,
		},
		{
			name:     "root path only",
			path:     `C:\`,
			expected: `C:\`,
		},
		{
			name:     "deep nested path",
			path:     `E:\some\deep\nested\path\file.txt`,
			expected: `E:\`,
		},
		{
			name:     "UNC path returns empty (unsupported)",
			path:     `\\server\share\path`,
			expected: "",
		},
		{
			name:     "empty path returns empty",
			path:     "",
			expected: "",
		},
		{
			name:     "relative path without volume returns empty",
			path:     `some\relative\path.txt`,
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := GetVolumeRoot(tc.path)
			if result != tc.expected {
				t.Errorf("GetVolumeRoot(%q) = %q, want %q", tc.path, result, tc.expected)
			}
		})
	}
}

func TestCheckDiskSpace_CurrentVolume(t *testing.T) {
	// Use the C:\ volume which should always exist on a Windows machine
	available, err := CheckDiskSpace(`C:\Windows\System32`)
	if err != nil {
		t.Fatalf("CheckDiskSpace failed: %v", err)
	}
	if available == 0 {
		t.Error("expected available disk space > 0 on C:\\ volume")
	}
}

func TestCheckDiskSpace_InvalidPath(t *testing.T) {
	// A relative path without a volume letter should return an error
	_, err := CheckDiskSpace("relative/path/file.txt")
	if err == nil {
		t.Error("expected error for path without volume root, got nil")
	}
}
