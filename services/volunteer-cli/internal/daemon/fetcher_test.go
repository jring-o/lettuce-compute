package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newFetcherTestDaemon(servers []*ServerConnection) *Daemon {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Defaults()
	cfg.DataDir = os.TempDir()
	cfg.Thermal.Enabled = false

	mr := &mockRuntime{canHandle: true}
	registry := NewRuntimeRegistry()
	registry.Register(mr)

	multiClient := NewMultiServerClient(servers, logger)
	leafCache := NewLeafCache(5*time.Minute, logger)
	ws := NewWeightedSelector()

	d := &Daemon{
		cfg:              cfg,
		pubKey:           pub,
		privKey:          priv,
		multiClient:      multiClient,
		runtimeRegistry:  registry,
		logger:           logger,
		initialBackoff:   10 * time.Millisecond,
		maxBackoff:       50 * time.Millisecond,
		cachedHW:         nil,
		leafCache:        leafCache,
		weightedSelector: ws,
		userPauseCh:      make(chan bool, 1),
	}

	// Set up leaf cache so weighted selection works.
	for _, srv := range servers {
		leafCache.PopulateForTest(srv.Name, &CachedHeadInfo{
			Name: srv.Name,
			Leafs: []CachedLeafInfo{
				{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
			},
			DefaultWeights: map[string]int{"leaf-1": 100},
		})
		ws.SetLeafWeights(srv.Name, map[string]int{"leaf-1": 100})
	}
	headWeights := make(map[string]int)
	for _, srv := range servers {
		headWeights[srv.Name] = 100
	}
	ws.SetHeadWeights(headWeights)

	return d
}

func TestFetcher_FillsQueue(t *testing.T) {
	wuCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			wuCount++
			return &lettucev1.RequestWorkUnitResponse{
				WorkUnitId:               fmt.Sprintf("00000000-0000-4000-8000-%012d", wuCount),
				ProjectId:                "proj-1",
				Runtime:                  "native",
				InputData:                []byte("input"),
				HeartbeatIntervalSeconds: 300,
				ExecutionSpec:            &lettucev1.ExecutionSpec{},
			}, nil
		},
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
				},
			}, nil
		},
	}

	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(4, d.logger)

	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 10 * time.Millisecond
	fetcher.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	go fetcher.Run(ctx)

	// Wait until queue reaches maxDepth.
	deadline := time.After(5 * time.Second)
	for {
		if queue.Len() >= 4 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("queue only reached %d items, wanted 4", queue.Len())
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()

	if queue.Len() < 4 {
		t.Errorf("queue length = %d, want >= 4", queue.Len())
	}
}

func TestFetcher_BacksOffWhenNoWork(t *testing.T) {
	callCount := 0
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			callCount++
			return nil, status.Error(codes.NotFound, "no work")
		},
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
				},
			}, nil
		},
	}

	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(2, d.logger)

	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 10 * time.Millisecond
	fetcher.maxBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	fetcher.Run(ctx)

	// Queue should still be empty.
	if queue.Len() != 0 {
		t.Errorf("queue length = %d, want 0", queue.Len())
	}

	// Should have been called multiple times (backoff, not stuck).
	if callCount < 2 {
		t.Errorf("server called %d times, expected > 1 (should retry with backoff)", callCount)
	}
}

func TestFetcher_StopsWhenContextCancelled(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(ctx context.Context, req *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
		getHeadInfoFn: func(ctx context.Context, req *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name: "server-a",
				Leafs: []*lettucev1.LeafInfo{
					{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"},
				},
			}, nil
		},
	}

	servers := []*ServerConnection{
		{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true},
	}

	d := newFetcherTestDaemon(servers)
	queue := NewPreFetchQueue(2, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	done := make(chan struct{})
	go func() {
		fetcher.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// OK — fetcher exited.
	case <-time.After(1 * time.Second):
		t.Fatal("fetcher did not stop within 1 second")
	}
}
