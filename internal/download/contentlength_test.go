package download

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetContentLength_ReturnsContentLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD request, got %s", r.Method)
		}
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	size, err := GetContentLength(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 1048576 {
		t.Errorf("expected 1048576, got %d", size)
	}
}

func TestGetContentLength_NonSuccessStatus(t *testing.T) {
	codes := []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError}
	for _, code := range codes {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer server.Close()

			size, err := GetContentLength(server.URL)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if size != -1 {
				t.Errorf("expected -1 for non-2xx status %d, got %d", code, size)
			}
		})
	}
}

func TestGetContentLength_NetworkError(t *testing.T) {
	// Use an invalid URL that will cause a network error
	size, err := GetContentLength("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != -1 {
		t.Errorf("expected -1 for network error, got %d", size)
	}
}

func TestGetContentLength_MissingHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't set Content-Length header
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	size, err := GetContentLength(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != -1 {
		t.Errorf("expected -1 for missing Content-Length, got %d", size)
	}
}

func TestGetContentLength_InvalidHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "not-a-number")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	size, err := GetContentLength(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != -1 {
		t.Errorf("expected -1 for invalid Content-Length, got %d", size)
	}
}

func TestGetContentLength_NegativeValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "-5")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	size, err := GetContentLength(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != -1 {
		t.Errorf("expected -1 for negative Content-Length, got %d", size)
	}
}

func TestGetContentLength_LargeFileSize(t *testing.T) {
	// 10 GB file
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10737418240")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	size, err := GetContentLength(server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 10737418240 {
		t.Errorf("expected 10737418240, got %d", size)
	}
}
