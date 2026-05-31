package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/resource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The disk gate must surface its otherwise-silent stall as a single WARN (not a
// per-poll Debug line), and re-arm so a later recovery + re-stall warns again.
func TestShouldFetch_DiskGateWarnsOnceAndRearms(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	lim := &thresholdLimiter{availMB: 50 * 1024}
	d := newTestDaemonWithResources(&mockClient{}, &mockRuntime{canHandle: true}, lim, scheduler)
	d.cfg.DataDir = t.TempDir()
	d.cfg.ResourceLimits.MaxDiskGB = 100 // needs 100*1024 MB free; only 50*1024 available → gated

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	for i := 0; i < 4; i++ {
		if d.shouldFetch() {
			t.Fatal("shouldFetch = true, want false while disk-gated")
		}
	}
	if got := strings.Count(buf.String(), "not fetching work"); got != 1 {
		t.Fatalf("disk-gate WARN count across repeated polls = %d, want exactly 1", got)
	}

	// Disk recovers → fetching resumes and the gate re-arms.
	lim.availMB = 1 << 30
	if !d.shouldFetch() {
		t.Fatal("shouldFetch = false, want true after disk recovered")
	}
	if !strings.Contains(buf.String(), "disk space recovered") {
		t.Error("expected a 'disk space recovered' message when the gate cleared")
	}

	// Re-stall → it warns again, proving the one-time flag re-armed.
	lim.availMB = 50 * 1024
	d.shouldFetch()
	if got := strings.Count(buf.String(), "not fetching work"); got != 2 {
		t.Errorf("disk-gate WARN count after re-stall = %d, want 2", got)
	}
}

// After a run of empty polls the fetcher must emit the "connected but getting no
// work" diagnostic exactly once, instead of leaving the daemon silently idle.
func TestFetcher_WarnsOnceWhenNoWork(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, status.Error(codes.NotFound, "no work")
		},
		getHeadInfoFn: func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
			return &lettucev1.GetHeadInfoResponse{
				Name:  "server-a",
				Leafs: []*lettucev1.LeafInfo{{Id: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"}},
			}, nil
		},
	}
	servers := []*ServerConnection{{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true}}
	d := newFetcherTestDaemon(servers)

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	queue := NewPreFetchQueue(2, d.logger)
	fetcher := NewFetcher(d, queue, d.weightedSelector, d.leafCache)
	fetcher.backoff = 5 * time.Millisecond
	fetcher.maxBackoff = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	fetcher.Run(ctx)

	if got := strings.Count(buf.String(), "connected but getting no work"); got != 1 {
		t.Errorf("no-work WARN count = %d, want exactly 1 (fires once past the empty-poll threshold)", got)
	}
}

// The startup readiness banner must escalate to a WARN when nothing is runnable
// — e.g. every attached leaf needs a container runtime this box doesn't have —
// so a misconfigured volunteer learns why instead of sitting silently idle.
func TestLogReadiness_WarnsWhenOnlyContainerLeafsAndNoRuntime(t *testing.T) {
	scheduler := resource.NewScheduler(&config.Scheduling{Mode: "ALWAYS"}, quietLogger())
	mc := &mockClient{}
	d := newTestDaemonWithResources(mc, &mockRuntime{canHandle: true}, &thresholdLimiter{availMB: 1 << 30}, scheduler)
	d.cfg.DataDir = t.TempDir()

	var buf bytes.Buffer
	d.logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Seed a container-only leaf; only a (non-container) mock runtime is registered.
	mc.getHeadInfoFn = func(_ context.Context, _ *lettucev1.GetHeadInfoRequest) (*lettucev1.GetHeadInfoResponse, error) {
		return &lettucev1.GetHeadInfoResponse{
			Leafs: []*lettucev1.LeafInfo{{
				Id:            "leaf-1",
				Slug:          "img-leaf",
				State:         "ACTIVE",
				ExecutionSpec: &lettucev1.ExecutionSpec{Image: "ghcr.io/example/img:1"},
			}},
		}, nil
	}
	if err := d.leafCache.Refresh(context.Background(), "default", mc); err != nil {
		t.Fatalf("seed leaf cache: %v", err)
	}

	d.logReadiness()

	s := buf.String()
	if !strings.Contains(s, "volunteer ready") {
		t.Error("expected the 'volunteer ready' banner")
	}
	if !strings.Contains(s, "no runnable leafs") {
		t.Errorf("expected a 'no runnable leafs' WARN for a container-only leaf with no container runtime; got: %s", s)
	}
}
