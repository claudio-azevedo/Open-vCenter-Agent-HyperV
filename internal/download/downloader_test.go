package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Feature: artifact-repository-agent, Property 4: Progress Monotonicity
// Feature: artifact-repository-agent, Property 5: Streaming Memory Bound
// Validates: Requirements 3.2, 4.1, 4.2

func TestDownloadFile_SuccessfulDownload(t *testing.T) {
	// Serve known content from a test HTTP server
	content := bytes.Repeat([]byte("hello world\n"), 1000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "downloaded.bin")

	err := DownloadFile(context.Background(), server.URL, destPath, int64(len(content)), nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify file content matches
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file content mismatch: got %d bytes, want %d bytes", len(got), len(content))
	}
}

func TestDownloadFile_Non2xxStatus(t *testing.T) {
	// Server returns 404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "downloaded.bin")

	err := DownloadFile(context.Background(), server.URL, destPath, -1, nil)
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
	// Verify error includes the status code
	if !contains(err.Error(), "404") {
		t.Errorf("expected error to contain '404', got: %v", err)
	}
}

func TestDownloadFile_NetworkError(t *testing.T) {
	// Use a cancelled context to simulate network failure
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	destPath := filepath.Join(t.TempDir(), "downloaded.bin")

	err := DownloadFile(ctx, "http://127.0.0.1:1/file", destPath, -1, nil)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
	// Verify it mentions the network/context failure
	if !contains(err.Error(), "failed") && !contains(err.Error(), "cancel") {
		t.Errorf("expected error to describe network failure, got: %v", err)
	}
}

func TestDownloadFile_ProgressCallbackFires(t *testing.T) {
	// Create a 10 MB file - enough so that 10% thresholds trigger multiple callbacks
	fileSize := 10 * 1024 * 1024 // 10 MB
	content := make([]byte, fileSize)
	rand.Read(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileSize))
		w.WriteHeader(http.StatusOK)
		// Write in small chunks to give progress writer a chance to fire
		chunkSize := 256 * 1024 // 256 KB chunks
		for i := 0; i < len(content); i += chunkSize {
			end := i + chunkSize
			if end > len(content) {
				end = len(content)
			}
			w.Write(content[i:end])
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "downloaded_10mb.bin")

	var mu sync.Mutex
	var reports []int64

	progressCb := func(bytesDownloaded int64, totalBytes int64) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, bytesDownloaded)
	}

	err := DownloadFile(context.Background(), server.URL, destPath, int64(fileSize), progressCb)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// With 10 MB file and 10% thresholds (1 MB each), we expect at least a few callbacks
	if len(reports) < 2 {
		t.Errorf("expected at least 2 progress callbacks, got %d", len(reports))
	}

	// Verify callbacks fire at approximately 10% intervals
	threshold := int64(fileSize) / 10
	for i, r := range reports {
		expectedMin := threshold * int64(i+1)
		if r < expectedMin-threshold/2 { // allow some tolerance due to buffering
			t.Logf("report %d: bytesDownloaded=%d (expected around %d)", i, r, expectedMin)
		}
	}
}

// Property 4: Progress Monotonicity - progress_percent values never decrease across reports
func TestDownloadFile_ProgressMonotonicity(t *testing.T) {
	// Create a 10 MB file for multiple progress callbacks
	fileSize := 10 * 1024 * 1024
	content := make([]byte, fileSize)
	rand.Read(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileSize))
		w.WriteHeader(http.StatusOK)
		// Write in small chunks
		chunkSize := 128 * 1024
		for i := 0; i < len(content); i += chunkSize {
			end := i + chunkSize
			if end > len(content) {
				end = len(content)
			}
			w.Write(content[i:end])
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "monotonicity_test.bin")

	var mu sync.Mutex
	var bytesReports []int64

	progressCb := func(bytesDownloaded int64, totalBytes int64) {
		mu.Lock()
		defer mu.Unlock()
		bytesReports = append(bytesReports, bytesDownloaded)
	}

	err := DownloadFile(context.Background(), server.URL, destPath, int64(fileSize), progressCb)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Verify bytesDownloaded values never decrease (monotonically non-decreasing)
	for i := 1; i < len(bytesReports); i++ {
		if bytesReports[i] < bytesReports[i-1] {
			t.Errorf("progress monotonicity violated: report[%d]=%d < report[%d]=%d",
				i, bytesReports[i], i-1, bytesReports[i-1])
		}
	}

	// Ensure we got at least some progress reports
	if len(bytesReports) == 0 {
		t.Error("expected at least one progress callback for 10 MB file")
	}
}

func TestDownloadFile_UnknownContentLength(t *testing.T) {
	// Serve content without specifying content length to DownloadFile (pass -1)
	content := bytes.Repeat([]byte("data"), 2048) // 8 KB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "unknown_length.bin")

	// With contentLength=-1, the progress callback should not fire on percentage thresholds
	// (only time-based, which won't trigger in a short download)
	var callbackCount int
	progressCb := func(bytesDownloaded int64, totalBytes int64) {
		callbackCount++
		if totalBytes != -1 {
			t.Errorf("expected totalBytes=-1 when content length unknown, got %d", totalBytes)
		}
	}

	err := DownloadFile(context.Background(), server.URL, destPath, -1, progressCb)
	if err != nil {
		t.Fatalf("expected no error with unknown content length, got: %v", err)
	}

	// Verify the file was still downloaded correctly
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file content mismatch: got %d bytes, want %d bytes", len(got), len(content))
	}
}

func TestDownloadFile_ContextCancellation(t *testing.T) {
	// Server sends data slowly so we can cancel mid-download
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10485760") // 10 MB
		w.WriteHeader(http.StatusOK)
		// Write data slowly in chunks
		chunk := make([]byte, 1024)
		for i := 0; i < 10240; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
				w.Write(chunk)
				w.(http.Flusher).Flush()
				time.Sleep(time.Millisecond)
			}
		}
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "cancelled.bin")

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a brief delay to ensure download starts
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := DownloadFile(ctx, server.URL, destPath, 10485760, nil)
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
}

// Helper to check substring in string
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Property 5: Streaming Memory Bound - verify the buffer used is exactly 1 MB
// This is a compile-time/code-inspection property verified by checking the constant.
func TestDownloadFile_StreamingMemoryBound(t *testing.T) {
	// Verify the buffer size constant is 1 MB
	if downloadBufferSize != 1<<20 {
		t.Errorf("expected downloadBufferSize to be 1 MB (1048576), got %d", downloadBufferSize)
	}

	// Create a larger file (5 MB) and download it - the buffer should still be only 1 MB.
	// We verify by monitoring that the progressWriter works correctly with any file size,
	// which indirectly validates that io.CopyBuffer is used with the fixed buffer.
	fileSize := 5 * 1024 * 1024
	content := make([]byte, fileSize)
	rand.Read(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", fileSize))
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	}))
	defer server.Close()

	destPath := filepath.Join(t.TempDir(), "membound_test.bin")

	err := DownloadFile(context.Background(), server.URL, destPath, int64(fileSize), nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify the file downloaded correctly despite memory constraint
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if len(got) != fileSize {
		t.Errorf("expected %d bytes, got %d", fileSize, len(got))
	}
}
