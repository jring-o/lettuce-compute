package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRefresh_StoresHeadInfo(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())

	mc := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name:        "Test Head",
				Description: "A test head",
				Url:         "https://test.example.com",
				Leafs: []*lettucev1.LeafInfo{
					{
						Id:               "leaf-1",
						Slug:             "prime-gaps",
						Name:             "Prime Gap Search",
						Description:      "Finding prime gaps",
						ResearchArea:     []string{"mathematics"},
						TaskPattern:      "PARAMETER_SWEEP",
						State:            "ACTIVE",
						QueuedWorkUnits:  42,
						ActiveVolunteers: 7,
					},
				},
				DefaultLeafWeights: map[string]int32{
					"prime-gaps": 200,
				},
			}, nil
		},
	}

	err := lc.Refresh(context.Background(), "srv-1", mc)
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}

	info := lc.GetHeadInfo("srv-1")
	if info == nil {
		t.Fatal("expected cached head info, got nil")
	}
	if info.Name != "Test Head" {
		t.Errorf("Name = %q, want Test Head", info.Name)
	}
	if len(info.Leafs) != 1 {
		t.Fatalf("Leafs count = %d, want 1", len(info.Leafs))
	}
	if info.Leafs[0].Slug != "prime-gaps" {
		t.Errorf("Slug = %q, want prime-gaps", info.Leafs[0].Slug)
	}
	if info.Leafs[0].QueuedWorkUnits != 42 {
		t.Errorf("QueuedWorkUnits = %d, want 42", info.Leafs[0].QueuedWorkUnits)
	}
	if info.DefaultWeights["prime-gaps"] != 200 {
		t.Errorf("DefaultWeights[prime-gaps] = %d, want 200", info.DefaultWeights["prime-gaps"])
	}
}

func TestRefresh_ErrorDoesNotClearCache(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())

	// First, store some data.
	goodClient := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "Original",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "test"},
				},
			}, nil
		},
	}
	if err := lc.Refresh(context.Background(), "srv-1", goodClient); err != nil {
		t.Fatal(err)
	}

	// Now refresh with a failing client.
	badClient := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	err := lc.Refresh(context.Background(), "srv-1", badClient)
	if err == nil {
		t.Fatal("expected error from bad client")
	}

	// Original data should still be there.
	info := lc.GetHeadInfo("srv-1")
	if info == nil {
		t.Fatal("expected cached info to survive failed refresh")
	}
	if info.Name != "Original" {
		t.Errorf("Name = %q, want Original", info.Name)
	}
}

func TestNeedsRefresh_TrueAfterInterval(t *testing.T) {
	// Use a very short refresh interval.
	lc := NewLeafCache(1*time.Millisecond, testLogger())

	mc := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{Name: "test"}, nil
		},
	}
	if err := lc.Refresh(context.Background(), "srv-1", mc); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * time.Millisecond)

	if !lc.NeedsRefresh("srv-1") {
		t.Error("expected NeedsRefresh=true after interval elapsed")
	}
}

func TestNeedsRefresh_FalseBeforeInterval(t *testing.T) {
	lc := NewLeafCache(1*time.Hour, testLogger())

	mc := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{Name: "test"}, nil
		},
	}
	if err := lc.Refresh(context.Background(), "srv-1", mc); err != nil {
		t.Fatal(err)
	}

	if lc.NeedsRefresh("srv-1") {
		t.Error("expected NeedsRefresh=false before interval elapsed")
	}
}

func TestGetLeafs_UnknownServer(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())
	leafs := lc.GetLeafs("nonexistent")
	if leafs != nil {
		t.Errorf("expected nil for unknown server, got %v", leafs)
	}
}

func TestRefreshAll_PartialFailure(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())

	goodClient := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{Name: "Good Server"}, nil
		},
	}
	badClient := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	servers := []*ServerConnection{
		{Name: "good", Client: goodClient, Available: true},
		{Name: "bad", Client: badClient, Available: true},
	}

	err := lc.RefreshAll(context.Background(), servers)
	if err != nil {
		t.Fatalf("RefreshAll should not return error on partial failure, got: %v", err)
	}

	if lc.GetHeadInfo("good") == nil {
		t.Error("good server should be cached")
	}
	if lc.GetHeadInfo("bad") != nil {
		t.Error("bad server should not be cached")
	}
}

func TestRefresh_CachesExecutionSpec(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())

	mc := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "WASM Head",
				Leafs: []*lettucev1.LeafInfo{
					{
						Id:   "leaf-wasm",
						Slug: "wasm-leaf",
						Name: "WASM Compute",
						ExecutionSpec: &lettucev1.ExecutionSpec{
							Binaries:      map[string]string{"wasm": "https://example.com/module.wasm"},
							GpuRequired:   true,
							GpuType:       "WEBGPU",
							MaxMemoryMb:   2048,
							MaxDiskMb:     5120,
							NetworkAccess: false,
						},
					},
					{
						Id:   "leaf-native",
						Slug: "native-leaf",
						Name: "Native Compute",
						ExecutionSpec: &lettucev1.ExecutionSpec{
							Binaries:    map[string]string{"linux-amd64": "sha256:abc"},
							Image:       "alpine:latest",
							MaxMemoryMb: 4096,
						},
					},
					{
						Id:   "leaf-no-spec",
						Slug: "no-spec-leaf",
						Name: "No Spec Leaf",
						// ExecutionSpec is nil
					},
				},
			}, nil
		},
	}

	err := lc.Refresh(context.Background(), "srv-wasm", mc)
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}

	info := lc.GetHeadInfo("srv-wasm")
	if info == nil {
		t.Fatal("expected cached head info")
	}
	if len(info.Leafs) != 3 {
		t.Fatalf("Leafs count = %d, want 3", len(info.Leafs))
	}

	// Verify WASM leaf ExecutionSpec.
	wasmLeaf := info.Leafs[0]
	if wasmLeaf.ExecutionSpec == nil {
		t.Fatal("WASM leaf ExecutionSpec should not be nil")
	}
	if wasmLeaf.ExecutionSpec.Binaries["wasm"] != "https://example.com/module.wasm" {
		t.Errorf("Binaries[wasm] = %q", wasmLeaf.ExecutionSpec.Binaries["wasm"])
	}
	if !wasmLeaf.ExecutionSpec.GPURequired {
		t.Error("GPURequired should be true")
	}
	if wasmLeaf.ExecutionSpec.GPUType != "WEBGPU" {
		t.Errorf("GPUType = %q, want WEBGPU", wasmLeaf.ExecutionSpec.GPUType)
	}
	if wasmLeaf.ExecutionSpec.MaxMemoryMB != 2048 {
		t.Errorf("MaxMemoryMB = %d, want 2048", wasmLeaf.ExecutionSpec.MaxMemoryMB)
	}
	if wasmLeaf.ExecutionSpec.NetworkAccess {
		t.Error("NetworkAccess should be false")
	}

	// Verify native leaf ExecutionSpec.
	nativeLeaf := info.Leafs[1]
	if nativeLeaf.ExecutionSpec == nil {
		t.Fatal("Native leaf ExecutionSpec should not be nil")
	}
	if nativeLeaf.ExecutionSpec.Image != "alpine:latest" {
		t.Errorf("Image = %q, want alpine:latest", nativeLeaf.ExecutionSpec.Image)
	}
	if nativeLeaf.ExecutionSpec.Binaries["linux-amd64"] != "sha256:abc" {
		t.Errorf("Binaries[linux-amd64] = %q", nativeLeaf.ExecutionSpec.Binaries["linux-amd64"])
	}

	// Verify leaf with nil ExecutionSpec.
	noSpecLeaf := info.Leafs[2]
	if noSpecLeaf.ExecutionSpec != nil {
		t.Errorf("no-spec leaf should have nil ExecutionSpec, got %+v", noSpecLeaf.ExecutionSpec)
	}
}

func TestConcurrentAccess(t *testing.T) {
	lc := NewLeafCache(5*time.Minute, testLogger())

	mc := &mockClient{
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "Concurrent",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "l1", Slug: "leaf"},
				},
			}, nil
		},
	}

	var wg sync.WaitGroup
	// Writers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = lc.Refresh(context.Background(), "srv", mc)
			}
		}()
	}
	// Readers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = lc.GetHeadInfo("srv")
				_ = lc.GetLeafs("srv")
				_ = lc.GetDefaultWeights("srv")
				_ = lc.NeedsRefresh("srv")
				_ = lc.AllLeafs()
			}
		}()
	}
	wg.Wait()
	// If we get here without a race condition panic, the test passes.
}
