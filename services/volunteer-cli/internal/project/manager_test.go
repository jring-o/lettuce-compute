package project

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := config.Defaults()
	cfg.DataDir = dir
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("saving config: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewManager(cfg, cfgPath, logger), dir
}

func TestAttachLeaf(t *testing.T) {
	mgr, _ := testManager(t)

	err := mgr.AttachLeaf("proj-1", "localhost:9090", "http://localhost:8080", "test-server")
	if err != nil {
		t.Fatalf("AttachLeaf: %v", err)
	}

	if len(mgr.cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(mgr.cfg.Servers))
	}
	s := mgr.cfg.Servers[0]
	if s.GRPCAddress != "localhost:9090" || len(s.PinnedLeafIDs) != 1 || s.PinnedLeafIDs[0] != "proj-1" {
		t.Errorf("server = %+v, want proj-1 pinned on localhost:9090", s)
	}
}

func TestAttachLeafDuplicate(t *testing.T) {
	mgr, _ := testManager(t)

	if err := mgr.AttachLeaf("proj-1", "localhost:9090", "http://localhost:8080", "test"); err != nil {
		t.Fatalf("first attach: %v", err)
	}

	err := mgr.AttachLeaf("proj-1", "localhost:9090", "http://localhost:8080", "test")
	if err == nil {
		t.Fatal("duplicate attach should fail")
	}
}

func TestAttachServer(t *testing.T) {
	mgr, _ := testManager(t)

	err := mgr.AttachServerWithTLS("example.com", 0, 0, false, "", []string{})
	if err != nil {
		t.Fatalf("AttachServer: %v", err)
	}

	if len(mgr.cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(mgr.cfg.Servers))
	}
	s := mgr.cfg.Servers[0]
	if s.GRPCAddress != "example.com:443" {
		t.Errorf("grpc address = %q, want example.com:443", s.GRPCAddress)
	}
	if s.HTTPAddress != "https://example.com" {
		t.Errorf("http address = %q, want https://example.com", s.HTTPAddress)
	}
	if s.LeafID != "" {
		t.Errorf("leaf id should be empty for server attach, got %q", s.LeafID)
	}
}

func TestAttachServerDuplicate(t *testing.T) {
	mgr, _ := testManager(t)

	if err := mgr.AttachServerWithTLS("example.com", 9090, 8080, false, "", []string{}); err != nil {
		t.Fatalf("first attach: %v", err)
	}

	err := mgr.AttachServerWithTLS("example.com", 9090, 8080, false, "", []string{})
	if err == nil {
		t.Fatal("duplicate server attach should fail")
	}
}

func TestAttachServerCustomPorts(t *testing.T) {
	mgr, _ := testManager(t)

	err := mgr.AttachServerWithTLS("example.com", 9091, 8081, false, "", []string{})
	if err != nil {
		t.Fatalf("AttachServer: %v", err)
	}

	if len(mgr.cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(mgr.cfg.Servers))
	}
	s := mgr.cfg.Servers[0]
	if s.GRPCAddress != "example.com:9091" {
		t.Errorf("grpc address = %q, want example.com:9091", s.GRPCAddress)
	}
	if s.HTTPAddress != "https://example.com:8081" {
		t.Errorf("http address = %q, want https://example.com:8081", s.HTTPAddress)
	}
}

func TestDetachLeaf(t *testing.T) {
	mgr, _ := testManager(t)

	if err := mgr.AttachLeaf("proj-1", "localhost:9090", "http://localhost:8080", "test"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	err := mgr.DetachLeaf("proj-1")
	if err != nil {
		t.Fatalf("DetachLeaf: %v", err)
	}

	// The head entry stays attached; only the pin is removed (PB-16 model).
	if len(mgr.cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1 (head entry stays)", len(mgr.cfg.Servers))
	}
	if len(mgr.cfg.Servers[0].PinnedLeafIDs) != 0 {
		t.Fatalf("pins = %v, want none", mgr.cfg.Servers[0].PinnedLeafIDs)
	}
}

func TestDetachLeafNotFound(t *testing.T) {
	mgr, _ := testManager(t)

	err := mgr.DetachLeaf("nonexistent")
	if err == nil {
		t.Fatal("detach nonexistent should fail")
	}
}

func TestListLeafsWithMockServer(t *testing.T) {
	leafs := []LeafSummary{
		{ID: "p1", Name: "Alpha", Slug: "alpha", ResearchArea: "physics", TaskPattern: "parameter_sweep", State: "ACTIVE"},
		{ID: "p2", Name: "Beta", Slug: "beta", ResearchArea: "biology", TaskPattern: "parameter_sweep", State: "ACTIVE"},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listLeafsResponse{Data: leafs}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	mgr, _ := testManager(t)
	result, err := mgr.ListLeafs(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("ListLeafs: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("leafs = %d, want 2", len(result))
	}
	if result[0].ID != "p1" || result[1].ID != "p2" {
		t.Errorf("unexpected leaf IDs: %v", result)
	}
}

func TestListLeafsFilterSpecific(t *testing.T) {
	leafs := []LeafSummary{
		{ID: "p1", Name: "Alpha"},
		{ID: "p2", Name: "Beta"},
		{ID: "p3", Name: "Gamma"},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listLeafsResponse{Data: leafs}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	mgr, _ := testManager(t)
	mgr.cfg.Leafs.Mode = "SPECIFIC"
	mgr.cfg.Leafs.LeafIDs = []string{"p1", "p3"}

	result, err := mgr.ListLeafs(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("ListLeafs: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("leafs = %d, want 2", len(result))
	}
	if result[0].ID != "p1" || result[1].ID != "p3" {
		t.Errorf("unexpected filtered results: %v", result)
	}
}

func TestListLeafsFilterBlocklist(t *testing.T) {
	leafs := []LeafSummary{
		{ID: "p1", Name: "Alpha"},
		{ID: "p2", Name: "Beta"},
		{ID: "p3", Name: "Gamma"},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listLeafsResponse{Data: leafs}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	mgr, _ := testManager(t)
	mgr.cfg.Leafs.Mode = "BLOCKLIST"
	mgr.cfg.Leafs.BlockedIDs = []string{"p2"}

	result, err := mgr.ListLeafs(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("ListLeafs: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("leafs = %d, want 2", len(result))
	}
	for _, p := range result {
		if p.ID == "p2" {
			t.Error("blocked leaf p2 should not be in results")
		}
	}
}

func TestListLeafsServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer ts.Close()

	mgr, _ := testManager(t)
	_, err := mgr.ListLeafs(context.Background(), ts.URL)
	if err == nil {
		t.Fatal("ListLeafs should fail on server error")
	}
}

func TestListLeafsPagination(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			// First page with cursor.
			resp := listLeafsResponse{
				Data: []LeafSummary{{ID: "p1", Name: "Alpha"}},
			}
			resp.Pagination.Cursor = "next-page"
			resp.Pagination.Total = 2
			json.NewEncoder(w).Encode(resp)
		} else {
			// Second page, no cursor.
			resp := listLeafsResponse{
				Data: []LeafSummary{{ID: "p2", Name: "Beta"}},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer ts.Close()

	mgr, _ := testManager(t)
	result, err := mgr.ListLeafs(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("ListLeafs: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("leafs = %d, want 2", len(result))
	}
	if result[0].ID != "p1" || result[1].ID != "p2" {
		t.Errorf("unexpected leaf IDs: %v", result)
	}
	if callCount != 2 {
		t.Errorf("expected 2 server calls, got %d", callCount)
	}
}

func TestGetStatusNoDaemon(t *testing.T) {
	mgr, _ := testManager(t)

	st, err := mgr.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.DaemonRunning {
		t.Error("expected daemon not running")
	}
}

func TestGetStatusWithDaemon(t *testing.T) {
	mgr, dir := testManager(t)

	// Write a PID file for the current process (which is running).
	if err := daemon.WritePID(dir); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	defer daemon.RemovePID(dir)

	st, err := mgr.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if !st.DaemonRunning {
		t.Error("expected daemon running (current process PID)")
	}
	if st.DaemonPID != os.Getpid() {
		t.Errorf("PID = %d, want %d", st.DaemonPID, os.Getpid())
	}
}

func TestGetHistory(t *testing.T) {
	mgr, dir := testManager(t)

	// Write some history entries.
	for i := 0; i < 5; i++ {
		entry := daemon.HistoryEntry{
			WorkUnitID:       fmt.Sprintf("wu-%d", i),
			LeafID:        "proj-1",
			CompletedAt:      time.Now().UTC().Add(time.Duration(i) * time.Minute),
			WallClockSeconds: int64(10 + i),
			ResultAccepted:   true,
		}
		if err := daemon.AppendHistory(dir, entry); err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
	}

	entries, err := mgr.GetHistory(context.Background(), 3)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	// Newest first.
	if entries[0].WorkUnitID != "wu-4" {
		t.Errorf("first entry = %q, want wu-4", entries[0].WorkUnitID)
	}
	if entries[2].WorkUnitID != "wu-2" {
		t.Errorf("last entry = %q, want wu-2", entries[2].WorkUnitID)
	}
}

func TestGetHistoryEmpty(t *testing.T) {
	mgr, _ := testManager(t)

	entries, err := mgr.GetHistory(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries for no history, got %d", len(entries))
	}
}

func TestDetachServer(t *testing.T) {
	mgr, _ := testManager(t)

	if err := mgr.AttachServerWithTLS("example.com", 9090, 8080, false, "", []string{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := mgr.AttachLeaf("p1", "example.com:9090", "http://example.com:8080", "example.com"); err != nil {
		t.Fatalf("attach leaf: %v", err)
	}

	// The pin merges into the one head entry (PB-16 model).
	if len(mgr.cfg.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(mgr.cfg.Servers))
	}

	if err := mgr.DetachServer("example.com"); err != nil {
		t.Fatalf("DetachServer: %v", err)
	}

	if len(mgr.cfg.Servers) != 0 {
		t.Fatalf("servers = %d, want 0 (should remove all matching host)", len(mgr.cfg.Servers))
	}
}

func TestDetachServerNotFound(t *testing.T) {
	mgr, _ := testManager(t)

	err := mgr.DetachServer("nonexistent.com")
	if err == nil {
		t.Fatal("detach nonexistent server should fail")
	}
}

func TestAttachLeafPersists(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := config.Defaults()
	cfg.DataDir = dir
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("saving config: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr := NewManager(cfg, cfgPath, logger)

	if err := mgr.AttachLeaf("proj-1", "localhost:9090", "http://localhost:8080", "test"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Reload config from disk.
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if len(loaded.Servers) != 1 {
		t.Fatalf("persisted servers = %d, want 1", len(loaded.Servers))
	}
	if len(loaded.Servers[0].PinnedLeafIDs) != 1 || loaded.Servers[0].PinnedLeafIDs[0] != "proj-1" {
		t.Errorf("persisted pins = %v, want [proj-1]", loaded.Servers[0].PinnedLeafIDs)
	}
}
