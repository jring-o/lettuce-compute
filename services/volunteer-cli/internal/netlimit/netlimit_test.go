package netlimit

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// withLimit sets the process-wide limit for one test and restores unlimited
// afterwards, so no other test in the package inherits it.
func withLimit(t *testing.T, mbps int) {
	t.Helper()
	SetMbps(mbps)
	t.Cleanup(func() { SetMbps(0) })
}

// loopbackPair returns a connected TCP pair on loopback: the client side is
// what a paced dial would return.
func loopbackPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

// minPacedDuration is the least time n bytes can take at mbps: everything past
// the one burst the bucket starts with has to wait for tokens.
func minPacedDuration(mbps, burst, n int) time.Duration {
	return time.Duration(float64(n-burst) / float64(BytesPerSecond(mbps)) * float64(time.Second))
}

func TestSetMbps_BurstIsATenthOfASecondWithinBounds(t *testing.T) {
	cases := []struct {
		mbps      int
		wantBurst int
	}{
		{1, minBurst},   // 12,500 B/tenth-second, below the floor
		{8, 100_000},    // 1 MB/s → 100 kB
		{100, maxBurst}, // 1.25 MB/tenth-second, above the ceiling
		{1023, maxBurst},
	}
	for _, c := range cases {
		withLimit(t, c.mbps)
		if got := Download.Burst(); got != c.wantBurst {
			t.Errorf("%d Mbps: download burst = %d, want %d", c.mbps, got, c.wantBurst)
		}
		if got := Upload.Burst(); got != c.wantBurst {
			t.Errorf("%d Mbps: upload burst = %d, want %d", c.mbps, got, c.wantBurst)
		}
		if Mbps() != c.mbps {
			t.Errorf("Mbps() = %d, want %d", Mbps(), c.mbps)
		}
	}
	SetMbps(0)
	if Download.Burst() != 0 || Upload.Burst() != 0 || Mbps() != 0 {
		t.Errorf("after SetMbps(0): bursts %d/%d, Mbps %d; want all 0", Download.Burst(), Upload.Burst(), Mbps())
	}
}

func TestTransferTimeAndAllowance(t *testing.T) {
	withLimit(t, 0)
	if got := Allowance(30*time.Second, 100<<20, 4<<20); got != 30*time.Second {
		t.Errorf("unlimited: Allowance = %s, want the base 30s unchanged", got)
	}
	if got := Download.TransferTime(1 << 30); got != 0 {
		t.Errorf("unlimited: TransferTime = %s, want 0", got)
	}

	withLimit(t, 10) // 1.25 MB/s each way
	if got, want := Upload.TransferTime(100_000_000), 80*time.Second; got != want {
		t.Errorf("100 MB at 10 Mbps: TransferTime = %s, want %s", got, want)
	}
	// 30s base + 2 × (80s up + 3.2s down).
	if got, want := Allowance(30*time.Second, 100_000_000, 4_000_000), 30*time.Second+2*(80*time.Second+3200*time.Millisecond); got != want {
		t.Errorf("Allowance = %s, want %s", got, want)
	}
}

func TestWrapConn_PacesWritesToTheLimit(t *testing.T) {
	const mbps = 2 // 250,000 B/s
	withLimit(t, mbps)
	client, server := loopbackPair(t)
	paced := WrapConn(client)

	payload := bytes.Repeat([]byte("u"), 500_000)
	received := make(chan int, 1)
	go func() {
		n, _ := io.Copy(io.Discard, server)
		received <- int(n)
	}()

	start := time.Now()
	if _, err := paced.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	elapsed := time.Since(start)
	paced.Close()
	if n := <-received; n != len(payload) {
		t.Fatalf("server received %d bytes, want %d", n, len(payload))
	}

	floor := minPacedDuration(mbps, Upload.Burst(), len(payload))
	if elapsed < floor {
		t.Errorf("500 kB written in %s at %d Mbps; pacing allows no less than %s", elapsed, mbps, floor)
	}
	if elapsed > 3*floor+time.Second {
		t.Errorf("500 kB took %s at %d Mbps; expected about %s — pacing is far slower than the limit", elapsed, mbps, floor)
	}
}

func TestWrapConn_PacesReadsToTheLimit(t *testing.T) {
	const mbps = 2
	withLimit(t, mbps)
	client, server := loopbackPair(t)
	paced := WrapConn(client)

	payload := bytes.Repeat([]byte("d"), 500_000)
	go func() {
		// The server side is not paced: it pushes as fast as the reader lets it.
		_, _ = server.Write(payload)
		server.Close()
	}()

	start := time.Now()
	n, err := io.Copy(io.Discard, paced)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int(n) != len(payload) {
		t.Fatalf("read %d bytes, want %d", n, len(payload))
	}
	floor := minPacedDuration(mbps, Download.Burst(), len(payload))
	if elapsed < floor {
		t.Errorf("500 kB read in %s at %d Mbps; pacing allows no less than %s", elapsed, mbps, floor)
	}
}

func TestWrapConn_UnlimitedIsNotPaced(t *testing.T) {
	withLimit(t, 0)
	client, server := loopbackPair(t)
	paced := WrapConn(client)

	payload := bytes.Repeat([]byte("x"), 4_000_000)
	go func() {
		_, _ = io.Copy(io.Discard, server)
	}()
	start := time.Now()
	if _, err := paced.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 4 MB over loopback takes milliseconds; even the lowest limit would need 32 s.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("4 MB unpaced took %s; with no limit set nothing should wait", elapsed)
	}
}

func TestWrapConn_ALimitSetLaterReachesAnOpenConnection(t *testing.T) {
	withLimit(t, 0)
	client, server := loopbackPair(t)
	paced := WrapConn(client) // wrapped while unlimited, as a long-lived head connection is
	go func() {
		_, _ = io.Copy(io.Discard, server)
	}()

	SetMbps(2)
	payload := bytes.Repeat([]byte("l"), 400_000)
	start := time.Now()
	if _, err := paced.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if elapsed, floor := time.Since(start), minPacedDuration(2, Upload.Burst(), len(payload)); elapsed < floor {
		t.Errorf("limit set after the connection opened: 400 kB took %s, want at least %s", elapsed, floor)
	}

	SetMbps(0)
	start = time.Now()
	if _, err := paced.Write(bytes.Repeat([]byte("l"), 4_000_000)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("limit lifted: 4 MB took %s; the open connection should no longer wait", elapsed)
	}
}

func TestDialContext_WrapsWhatItDials(t *testing.T) {
	client, _ := loopbackPair(t)
	dial := DialContext(func(ctx context.Context, network, address string) (net.Conn, error) {
		return client, nil
	})
	c, err := dial(context.Background(), "tcp", "ignored")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, ok := c.(*pacedConn); !ok {
		t.Fatalf("DialContext returned %T, want a paced connection", c)
	}
	if again := WrapConn(c); again != c {
		t.Error("WrapConn wrapped an already-paced connection a second time")
	}
}
