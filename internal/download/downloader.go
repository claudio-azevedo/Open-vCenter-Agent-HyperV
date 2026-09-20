package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const downloadBufferSize = 1 << 20 // 1 MB

// progressWriter wraps an io.Writer and calls a progress callback after each write,
// reporting the cumulative bytes written and the expected total.
// The callback fires only when 10% of total bytes has been written since the last
// report OR 30 seconds have elapsed since the last report, whichever comes first.
// When totalBytes is unknown (<= 0), the callback fires only on the 30-second interval.
type progressWriter struct {
	writer            io.Writer
	totalBytes        int64
	written           int64
	lastReportedBytes int64
	lastReportTime    time.Time
	progressCb        func(bytesDownloaded int64, totalBytes int64)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.writer.Write(p)
	pw.written += int64(n)

	if pw.progressCb != nil {
		shouldReport := false

		if pw.totalBytes > 0 {
			// Known size: fire when 10% of total has been written since last report
			threshold := pw.totalBytes / 10
			if pw.written-pw.lastReportedBytes >= threshold {
				shouldReport = true
			}
		}

		// Always check time-based threshold (30 seconds)
		if time.Since(pw.lastReportTime) >= 30*time.Second {
			shouldReport = true
		}

		if shouldReport {
			pw.lastReportedBytes = pw.written
			pw.lastReportTime = time.Now()
			pw.progressCb(pw.written, pw.totalBytes)
		}
	}

	return n, err
}

// DownloadFile downloads the content from url into a file at destTmpPath, streaming
// via a fixed 1 MB buffer. The progressCb is called with the cumulative bytes downloaded
// and the total expected bytes (contentLength). The context can be used for cancellation.
//
// Returns an error if the HTTP response status is not 2xx, or if any network/IO error occurs.
func DownloadFile(ctx context.Context, url string, destTmpPath string, contentLength int64, progressCb func(bytesDownloaded int64, totalBytes int64)) error {
	// Create the destination temp file
	f, err := os.Create(destTmpPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file %s: %w", destTmpPath, err)
	}
	defer f.Close()

	// Build HTTP request with context for cancellation support
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	// Execute the request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET request failed: %w", err)
	}
	defer resp.Body.Close()

	// Validate response status is 2xx
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed with HTTP status %d", resp.StatusCode)
	}

	// Wrap the file writer with progress tracking
	pw := &progressWriter{
		writer:         f,
		totalBytes:     contentLength,
		lastReportTime: time.Now(),
		progressCb:     progressCb,
	}

	// Stream from response body to file using a 1 MB buffer
	buf := make([]byte, downloadBufferSize)
	if _, err := io.CopyBuffer(pw, resp.Body, buf); err != nil {
		return fmt.Errorf("failed to write download to disk: %w", err)
	}

	return nil
}
