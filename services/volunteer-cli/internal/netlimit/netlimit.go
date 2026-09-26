// Package netlimit paces the network transfers this process makes itself to the
// volunteer's resource_limits.max_bandwidth_mbps.
//
// The figure is one number of megabits per second and applies to each direction
// separately: everything the process downloads, together, stays under it, and
// everything it uploads, together, stays under it. A link is full-duplex, so a
// result upload does not need to slow an artifact download to respect the
// figure. 0 means unlimited, which is the default.
//
// Pacing happens on the connection, not on a particular transfer: every
// connection this client dials to a head (gRPC) or to an artifact host (HTTP)
// is wrapped by WrapConn, and each read or write draws from one process-wide
// token bucket per direction. The buckets are consulted on every read and
// write, so a changed limit reaches connections that are already open,
// including the head connection a daemon keeps for its whole life.
//
// What this cannot reach: container image pulls (the container engine fetches
// the layers itself, and neither Docker nor Podman offers a download rate
// setting), the desktop app's own update check and download, and anything a
// leaf's own code does inside its sandbox.
package netlimit

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Direction is one direction of traffic, paced by its own token bucket. The
// zero value is unlimited.
type Direction struct {
	// lim is nil while the direction is unlimited, so the common case costs one
	// atomic load per read or write. A changed limit installs a fresh bucket
	// rather than reconfiguring the old one in place.
	lim  atomic.Pointer[rate.Limiter]
	mbps atomic.Int64
}

// Download paces what this process reads from the network; Upload what it
// writes.
var (
	Download = &Direction{}
	Upload   = &Direction{}
)

// minBurst and maxBurst bound how many bytes one read or write may move before
// it waits. The burst is a tenth of a second of the rate within these bounds:
// small enough that a transfer never runs far ahead of the limit and a wait is
// never long, large enough that a fast limit does not spend its time waking up.
const (
	minBurst = 16 * 1024
	maxBurst = 1024 * 1024
)

// SetMbps sets the limit for both directions, in megabits per second. 0 or less
// removes it.
func SetMbps(mbps int) {
	Download.set(mbps)
	Upload.set(mbps)
}

// Mbps reports the limit in force, 0 when unlimited.
func Mbps() int { return int(Download.mbps.Load()) }

func (d *Direction) set(mbps int) {
	if mbps <= 0 {
		d.mbps.Store(0)
		d.lim.Store(nil)
		return
	}
	bytesPerSec := BytesPerSecond(mbps)
	burst := int(bytesPerSec / 10)
	if burst < minBurst {
		burst = minBurst
	}
	if burst > maxBurst {
		burst = maxBurst
	}
	d.mbps.Store(int64(mbps))
	d.lim.Store(rate.NewLimiter(rate.Limit(bytesPerSec), burst))
}

// BytesPerSecond converts a megabits-per-second figure to bytes per second, the
// unit the buckets count in. One megabit is 1,000,000 bits, as network speeds
// are quoted.
func BytesPerSecond(mbps int) int64 {
	return int64(mbps) * 1_000_000 / 8
}

// Burst reports how many bytes one read or write may move before it waits, or
// 0 when the direction is unlimited.
func (d *Direction) Burst() int {
	if lim := d.lim.Load(); lim != nil {
		return lim.Burst()
	}
	return 0
}

// TransferTime is how long n bytes take in this direction at the limit in
// force, or 0 when it is unlimited.
func (d *Direction) TransferTime(n int64) time.Duration {
	mbps := d.mbps.Load()
	if mbps <= 0 || n <= 0 {
		return 0
	}
	return time.Duration(float64(n) / float64(BytesPerSecond(int(mbps))) * float64(time.Second))
}

// allowanceMargin stretches a paced transfer's expected time so a deadline sized
// with Allowance tolerates protocol overhead (TLS records, HTTP/2 and gRPC
// framing) and a second transfer sharing the same direction's bucket.
const allowanceMargin = 2

// Allowance returns base plus the time up bytes of upload and down bytes of
// download take at the limit in force, with a margin. It is how a fixed
// deadline written for an unpaced link is widened so that pacing alone never
// turns a transfer into a timeout; with no limit set it returns base unchanged.
func Allowance(base time.Duration, up, down int64) time.Duration {
	return base + allowanceMargin*(Upload.TransferTime(up)+Download.TransferTime(down))
}

// wait blocks until n bytes may move in this direction. It never fails: the
// callers are a connection's Read and Write, which have no context, and each
// step waits at most one burst's worth of time behind the transfers already
// queued on the bucket.
func (d *Direction) wait(lim *rate.Limiter, n int) {
	for n > 0 {
		step := n
		if b := lim.Burst(); step > b {
			step = b
		}
		_ = lim.WaitN(context.Background(), step)
		n -= step
	}
}

// WrapConn returns c paced: its reads by Download and its writes by Upload.
// Wrap every connection whether or not a limit is set now, so that a limit set
// later reaches it.
func WrapConn(c net.Conn) net.Conn {
	if c == nil {
		return nil
	}
	if _, already := c.(*pacedConn); already {
		return c
	}
	return &pacedConn{Conn: c}
}

// DialContext wraps a dial function so every connection it returns is paced.
func DialContext(dial func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return WrapConn(c), nil
	}
}

type pacedConn struct {
	net.Conn
}

// Read reads at most one burst, then waits for the bytes it actually read.
// Waiting after the read rather than before charges exactly what arrived; the
// sender is slowed by TCP flow control while this reader waits.
func (c *pacedConn) Read(p []byte) (int, error) {
	lim := Download.lim.Load()
	if lim == nil {
		return c.Conn.Read(p)
	}
	if b := lim.Burst(); len(p) > b {
		p = p[:b]
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		Download.wait(lim, n)
	}
	return n, err
}

// Write sends p one burst at a time, waiting before each.
func (c *pacedConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		lim := Upload.lim.Load()
		if lim == nil {
			n, err := c.Conn.Write(p)
			return written + n, err
		}
		step := len(p)
		if b := lim.Burst(); step > b {
			step = b
		}
		Upload.wait(lim, step)
		n, err := c.Conn.Write(p[:step])
		written += n
		if err != nil {
			return written, err
		}
		p = p[step:]
	}
	return written, nil
}
