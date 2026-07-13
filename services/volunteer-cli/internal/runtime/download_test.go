package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCopyCapped is the BG-16d exit test (j) mechanism: a download is refused once it
// exceeds the size cap. native (downloadFile) and wasm (downloadToFile) both stream
// their artifact through copyCapped, so an infinite/oversized artifact URL cannot
// fill the volunteer's disk during the download.
func TestCopyCapped(t *testing.T) {
	// Under the cap: copies fully.
	var buf bytes.Buffer
	n, err := copyCapped(&buf, bytes.NewReader(make([]byte, 100)), 1000)
	if err != nil {
		t.Fatalf("copyCapped under cap: %v", err)
	}
	if n != 100 || buf.Len() != 100 {
		t.Errorf("copyCapped copied %d bytes (buf %d), want 100", n, buf.Len())
	}

	// Over the cap: refused.
	buf.Reset()
	if _, err := copyCapped(&buf, bytes.NewReader(make([]byte, 2000)), 1000); err == nil {
		t.Error("copyCapped accepted input larger than the cap; want an error")
	}

	// Exactly at the cap: allowed.
	buf.Reset()
	if _, err := copyCapped(&buf, bytes.NewReader(make([]byte, 1000)), 1000); err != nil {
		t.Errorf("copyCapped at exactly the cap: %v, want nil", err)
	}
}

func TestDownloadExternalData_HappyPath(t *testing.T) {
	payload := []byte("hello external data")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	data, checksum, err := DownloadExternalDataWithClient(context.Background(), srv.Client(), srv.URL, 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != string(payload) {
		t.Errorf("data = %q, want %q", data, payload)
	}

	hash := sha256.Sum256(payload)
	wantChecksum := hex.EncodeToString(hash[:])
	if checksum != wantChecksum {
		t.Errorf("checksum = %s, want %s", checksum, wantChecksum)
	}
}

func TestDownloadExternalData_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, _, err := DownloadExternalDataWithClient(context.Background(), srv.Client(), srv.URL, 1024)
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
}

func TestDownloadExternalData_OversizedResponse(t *testing.T) {
	// Server returns 100 bytes, but maxBytes is 50.
	payload := make([]byte, 100)
	for i := range payload {
		payload[i] = 'A'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	_, _, err := DownloadExternalDataWithClient(context.Background(), srv.Client(), srv.URL, 50)
	if err == nil {
		t.Fatal("expected error for oversized response")
	}
}

func TestDownloadExternalData_ContentLengthExceedsMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000")
		w.Write([]byte("small"))
	}))
	defer srv.Close()

	_, _, err := DownloadExternalDataWithClient(context.Background(), srv.Client(), srv.URL, 100)
	if err == nil {
		t.Fatal("expected error for content-length exceeding max")
	}
}

func TestDownloadExternalData_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte("too late"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := DownloadExternalDataWithClient(ctx, srv.Client(), srv.URL, 1024)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestDownloadExternalData_RedirectChain(t *testing.T) {
	payload := []byte("final destination")
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			w.Write(payload)
			return
		}
		hops++
		next := fmt.Sprintf("/hop%d", hops)
		if hops >= 3 {
			next = "/final"
		}
		http.Redirect(w, r, next, http.StatusFound)
	}))
	defer srv.Close()

	data, _, err := DownloadExternalDataWithClient(context.Background(), srv.Client(), srv.URL+"/start", 1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != string(payload) {
		t.Errorf("data = %q, want %q", data, payload)
	}
}

func TestDownloadExternalData_TooManyRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()

	_, _, err := DownloadExternalData(context.Background(), srv.URL+"/start", 1024)
	if err == nil {
		t.Fatal("expected error for too many redirects")
	}
}

func TestDownloadExternalData_EmptyURL(t *testing.T) {
	_, _, err := DownloadExternalData(context.Background(), "", 1024)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}
