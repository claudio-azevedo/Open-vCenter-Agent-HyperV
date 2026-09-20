package download

import (
	"net/http"
	"strconv"
)

// GetContentLength performs an HTTP HEAD request to the given URL and returns
// the Content-Length header value as an int64.
//
// If the HEAD request fails (network error), returns a non-2xx status, or the
// Content-Length header is missing or not a valid integer, it returns -1 and nil.
// A return value of -1 signals that the file size is unknown and the caller
// should skip any disk space pre-check.
func GetContentLength(url string) (int64, error) {
	resp, err := http.DefaultClient.Head(url)
	if err != nil {
		// Network error - size unknown, skip disk check
		return -1, nil
	}
	defer resp.Body.Close()

	// Non-2xx status - skip disk check
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return -1, nil
	}

	// Parse Content-Length header
	clHeader := resp.Header.Get("Content-Length")
	if clHeader == "" {
		return -1, nil
	}

	contentLength, err := strconv.ParseInt(clHeader, 10, 64)
	if err != nil || contentLength < 0 {
		return -1, nil
	}

	return contentLength, nil
}
