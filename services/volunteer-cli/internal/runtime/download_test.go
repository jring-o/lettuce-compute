package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
