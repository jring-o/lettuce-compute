package runtime

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/netlimit"
)

// An artifact download through the production client is held to
// resource_limits.max_bandwidth_mbps, and the client's fixed whole-request
// timeout is widened so the paced download still completes. The unguarded
// artifact client is the one that may reach loopback; it is built exactly like
// the guarded one apart from the dial screen, so its transport is the one under
// test. Its timeout is shortened to one second — the test-sized stand-in for
// the five minutes that do not fit a large artifact at a low limit.
func TestArtifactDownload_PacedToTheBandwidthLimitWithoutTimingOut(t *testing.T) {
	const mbps = 2 // 250,000 bytes a second
	netlimit.SetMbps(mbps)
	t.Cleanup(func() { netlimit.SetMbps(0) })

	body := bytes.Repeat([]byte("a"), 600_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := *unguardedArtifactClient()
	client.Timeout = time.Second

	start := time.Now()
	data, _, err := DownloadExternalDataWithClient(context.Background(), &client, srv.URL, 700_000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("download under a %d Mbps limit failed after %s: %v (a paced download must not time out)", mbps, elapsed, err)
	}
	if len(data) != len(body) {
		t.Fatalf("downloaded %d bytes, want %d", len(data), len(body))
	}
	floor := time.Duration(float64(len(body)-netlimit.Download.Burst()) / float64(netlimit.BytesPerSecond(mbps)) * float64(time.Second))
	if elapsed < floor {
		t.Errorf("600 kB downloaded in %s under a %d Mbps limit; the limit allows no less than %s", elapsed, mbps, floor)
	}
}

func TestClientForTransfer(t *testing.T) {
	base := &http.Client{Timeout: 5 * time.Minute}

	netlimit.SetMbps(0)
	if got := clientForTransfer(base, DefaultMaxArtifactBytes); got != base {
		t.Error("unlimited: the client must be returned as it is")
	}

	netlimit.SetMbps(10)
	t.Cleanup(func() { netlimit.SetMbps(0) })
	got := clientForTransfer(base, DefaultMaxArtifactBytes)
	if got == base {
		t.Fatal("10 Mbps: a 2 GiB download does not fit in 5 minutes; the timeout must be widened on a copy")
	}
	if base.Timeout != 5*time.Minute {
		t.Errorf("the shared client was modified: timeout %s", base.Timeout)
	}
	// 2 GiB at 1.25 MB/s is about 28.6 minutes before the margin.
	if got.Timeout < 5*time.Minute+2*28*time.Minute {
		t.Errorf("widened timeout %s is too short for 2 GiB at 10 Mbps", got.Timeout)
	}
	if got.Transport != base.Transport {
		t.Error("the widened copy must share the original transport")
	}

	noTimeout := &http.Client{}
	if clientForTransfer(noTimeout, DefaultMaxArtifactBytes) != noTimeout {
		t.Error("a client with no timeout has nothing to widen")
	}
}
