package server

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// peerCtx builds a context carrying a gRPC peer with the given "host:port"
// address, plus optional x-forwarded-for / x-real-ip incoming metadata.
func peerCtx(addr, xff, xRealIP string) context.Context {
	ctx := context.Background()
	if addr != "" {
		ctx = peer.NewContext(ctx, &peer.Peer{Addr: &net.TCPAddr{
			IP:   net.ParseIP(hostOnly(addr)),
			Port: portOnly(addr),
		}})
	}
	pairs := []string{}
	if xff != "" {
		pairs = append(pairs, "x-forwarded-for", xff)
	}
	if xRealIP != "" {
		pairs = append(pairs, "x-real-ip", xRealIP)
	}
	if len(pairs) > 0 {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(pairs...))
	}
	return ctx
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func portOnly(addr string) int {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		// errors ignored: test inputs are well-formed; 0 is fine otherwise.
		var n int
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	return 0
}

// TestGRPCClientIP_NoTrust verifies that with no trusted proxies, grpcClientIP
// always returns the direct peer IP and never honors spoofable x-forwarded-for /
// x-real-ip metadata — mirroring clientIPFromRequest's secure default.
func TestGRPCClientIP_NoTrust(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		xff     string
		xRealIP string
		wantIP  string
	}{
		{name: "peer with port", addr: "192.168.1.1:12345", wantIP: "192.168.1.1"},
		{name: "XFF ignored (untrusted peer)", addr: "10.0.0.1:1234", xff: "203.0.113.50", wantIP: "10.0.0.1"},
		{name: "XFF chain ignored", addr: "10.0.0.1:1234", xff: "203.0.113.50, 70.41.3.18, 150.172.238.178", wantIP: "10.0.0.1"},
		{name: "X-Real-IP ignored", addr: "10.0.0.1:1234", xRealIP: "198.51.100.1", wantIP: "10.0.0.1"},
		{name: "both ignored", addr: "10.0.0.1:1234", xff: "203.0.113.50", xRealIP: "198.51.100.1", wantIP: "10.0.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := peerCtx(tt.addr, tt.xff, tt.xRealIP)
			if got := grpcClientIP(ctx, nil); got != tt.wantIP {
				t.Fatalf("grpcClientIP(nil) = %q, want %q", got, tt.wantIP)
			}
		})
	}
}

// TestGRPCClientIP_TrustedPeer verifies trust-aware extraction when the direct
// gRPC peer IS a configured trusted proxy: walk x-forwarded-for right-to-left,
// skipping trusted hops, returning the right-most non-trusted entry; fall back to
// x-real-ip then the peer IP.
func TestGRPCClientIP_TrustedPeer(t *testing.T) {
	trusted := []*net.IPNet{
		mustCIDR(t, "10.0.0.0/8"),
		mustCIDR(t, "172.16.0.0/12"),
	}

	tests := []struct {
		name    string
		addr    string
		xff     string
		xRealIP string
		wantIP  string
	}{
		{name: "single client in XFF", addr: "10.0.0.1:1234", xff: "203.0.113.50", wantIP: "203.0.113.50"},
		{name: "right-most non-trusted wins", addr: "10.0.0.1:1234", xff: "203.0.113.50, 70.41.3.18, 10.0.0.9", wantIP: "70.41.3.18"},
		{name: "multiple trusted hops skipped", addr: "10.0.0.1:1234", xff: "203.0.113.50, 172.16.5.5, 10.0.0.9", wantIP: "203.0.113.50"},
		{name: "spoofed left entry does NOT win", addr: "10.0.0.1:1234", xff: "1.2.3.4, 70.41.3.18, 10.0.0.9", wantIP: "70.41.3.18"},
		{name: "all XFF trusted falls back to X-Real-IP", addr: "10.0.0.1:1234", xff: "10.0.0.9, 172.16.1.1", xRealIP: "198.51.100.7", wantIP: "198.51.100.7"},
		{name: "no XFF uses X-Real-IP", addr: "10.0.0.1:1234", xRealIP: "198.51.100.7", wantIP: "198.51.100.7"},
		{name: "no usable headers uses peer IP", addr: "10.0.0.1:1234", wantIP: "10.0.0.1"},
		{name: "invalid XFF entries skipped", addr: "10.0.0.1:1234", xff: "not-an-ip, 203.0.113.50, garbage", wantIP: "203.0.113.50"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := peerCtx(tt.addr, tt.xff, tt.xRealIP)
			if got := grpcClientIP(ctx, trusted); got != tt.wantIP {
				t.Fatalf("grpcClientIP(trusted) = %q, want %q", got, tt.wantIP)
			}
		})
	}
}

// TestGRPCClientIP_UntrustedPeerIgnoresMetadata verifies that when the direct
// peer is NOT in the trusted set, forwarding metadata is ignored even though some
// trusted proxies are configured.
func TestGRPCClientIP_UntrustedPeerIgnoresMetadata(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}

	ctx := peerCtx("203.0.113.99:5555", "1.2.3.4", "5.6.7.8") // public peer, NOT trusted
	if got := grpcClientIP(ctx, trusted); got != "203.0.113.99" {
		t.Fatalf("untrusted peer should ignore metadata: got %q, want %q", got, "203.0.113.99")
	}
}

// TestGRPCClientIP_MultipleXFFMetadataValues verifies that repeated
// x-forwarded-for metadata values are comma-joined into one hop chain before the
// right-to-left walk, so a client IP carried in an earlier metadata value is still
// found.
func TestGRPCClientIP_MultipleXFFMetadataValues(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}

	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1234}})
	// Two separate metadata values; the real client (203.0.113.50) is in the first.
	md := metadata.MD{}
	md.Append("x-forwarded-for", "203.0.113.50")
	md.Append("x-forwarded-for", "10.0.0.9")
	ctx = metadata.NewIncomingContext(ctx, md)

	if got := grpcClientIP(ctx, trusted); got != "203.0.113.50" {
		t.Fatalf("joined XFF metadata = %q, want %q", got, "203.0.113.50")
	}
}

// TestGRPCClientIP_NoPeer verifies the unknown fallback when no peer is present.
func TestGRPCClientIP_NoPeer(t *testing.T) {
	if got := grpcClientIP(context.Background(), nil); got != "unknown" {
		t.Fatalf("no peer = %q, want %q", got, "unknown")
	}
}

// --- per-pubkey limiter tests ---

// okHandler is a trivial gRPC handler that records that it ran.
func okHandler(ran *bool) grpc.UnaryHandler {
	return func(ctx context.Context, req any) (any, error) {
		*ran = true
		return "ok", nil
	}
}

func newPubkey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub
}

// TestGRPCPerPubkeyRateLimit_IndependentBuckets verifies two distinct pubkeys
// (conceptually from one IP) get independent per-volunteer buckets: exhausting
// one does not throttle the other.
func TestGRPCPerPubkeyRateLimit_IndependentBuckets(t *testing.T) {
	old := grpcPerPubkeyRateLimit
	grpcPerPubkeyRateLimit = 3
	defer func() { grpcPerPubkeyRateLimit = old }()

	store := newRateLimitStore()
	interceptor := grpcPerPubkeyRateLimitInterceptor(store, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.volunteer.v1.VolunteerService/RequestWorkUnit"}

	pkA := newPubkey(t)
	pkB := newPubkey(t)

	call := func(pk ed25519.PublicKey) error {
		ctx := contextWithGRPCAuthPublicKey(context.Background(), pk)
		var ran bool
		_, err := interceptor(ctx, nil, info, okHandler(&ran))
		return err
	}

	// Exhaust pkA's budget (3 allowed, 4th rejected).
	for i := 0; i < grpcPerPubkeyRateLimit; i++ {
		if err := call(pkA); err != nil {
			t.Fatalf("pkA call %d: unexpected error %v", i+1, err)
		}
	}
	if err := call(pkA); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("pkA over budget: want ResourceExhausted, got %v", err)
	}

	// pkB has an independent bucket and must still pass.
	if err := call(pkB); err != nil {
		t.Fatalf("pkB should not be throttled by pkA: got %v", err)
	}

	// Sanity: the buckets are keyed on the pubkey hex.
	if _, ok := store.buckets["pubkey:"+hex.EncodeToString(pkA)]; !ok {
		t.Fatal("expected a bucket keyed on pkA")
	}
	if _, ok := store.buckets["pubkey:"+hex.EncodeToString(pkB)]; !ok {
		t.Fatal("expected a bucket keyed on pkB")
	}
}

// TestGRPCPerPubkeyRateLimit_PublicMethodNeverThrottled verifies that a context
// with no verified pubkey (public discovery methods) bypasses the per-volunteer
// limiter entirely — even when the limit is tiny.
func TestGRPCPerPubkeyRateLimit_PublicMethodNeverThrottled(t *testing.T) {
	old := grpcPerPubkeyRateLimit
	grpcPerPubkeyRateLimit = 1
	defer func() { grpcPerPubkeyRateLimit = old }()

	store := newRateLimitStore()
	interceptor := grpcPerPubkeyRateLimitInterceptor(store, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.volunteer.v1.VolunteerService/GetServerStatus"}

	for i := 0; i < 10; i++ {
		ctx := context.Background() // no pubkey in context
		var ran bool
		if _, err := interceptor(ctx, nil, info, okHandler(&ran)); err != nil {
			t.Fatalf("public-method call %d should never be throttled: got %v", i+1, err)
		}
		if !ran {
			t.Fatalf("public-method call %d: handler did not run", i+1)
		}
	}
	if len(store.buckets) != 0 {
		t.Fatalf("public methods should mint no per-pubkey buckets, got %d", len(store.buckets))
	}
}

// TestGRPCPerPubkeyRateLimit_RejectsOverBudget verifies a single volunteer that
// exceeds its budget is rejected with ResourceExhausted and the handler does not
// run for the rejected call.
func TestGRPCPerPubkeyRateLimit_RejectsOverBudget(t *testing.T) {
	old := grpcPerPubkeyRateLimit
	grpcPerPubkeyRateLimit = 2
	defer func() { grpcPerPubkeyRateLimit = old }()

	store := newRateLimitStore()
	interceptor := grpcPerPubkeyRateLimitInterceptor(store, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.volunteer.v1.VolunteerService/RequestWorkUnit"}
	pk := newPubkey(t)

	for i := 0; i < grpcPerPubkeyRateLimit; i++ {
		ctx := contextWithGRPCAuthPublicKey(context.Background(), pk)
		var ran bool
		if _, err := interceptor(ctx, nil, info, okHandler(&ran)); err != nil || !ran {
			t.Fatalf("call %d: err=%v ran=%v", i+1, err, ran)
		}
	}

	ctx := contextWithGRPCAuthPublicKey(context.Background(), pk)
	var ran bool
	_, err := interceptor(ctx, nil, info, okHandler(&ran))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over budget: want ResourceExhausted, got %v", err)
	}
	if ran {
		t.Fatal("handler must not run for a rejected call")
	}
}

// TestSetGRPCRateLimits verifies the operator override: positive values raise the
// per-IP / per-pubkey budgets, and a non-positive value leaves the corresponding
// limit at its current value. This is the production-facing knob for fleets behind
// one NAT (or load tests) that would otherwise share a single 60/min IP bucket.
func TestSetGRPCRateLimits(t *testing.T) {
	oldIP, oldPK := grpcRateLimit, grpcPerPubkeyRateLimit
	defer func() { grpcRateLimit, grpcPerPubkeyRateLimit = oldIP, oldPK }()

	grpcRateLimit, grpcPerPubkeyRateLimit = 60, 120

	// Both raised together.
	SetGRPCRateLimits(6000, 12000)
	if grpcRateLimit != 6000 {
		t.Fatalf("per-IP = %d, want 6000", grpcRateLimit)
	}
	if grpcPerPubkeyRateLimit != 12000 {
		t.Fatalf("per-pubkey = %d, want 12000", grpcPerPubkeyRateLimit)
	}

	// Zero / negative leaves the corresponding limit untouched (each env override
	// in main.go is applied independently as SetGRPCRateLimits(n, 0) / (0, n)).
	SetGRPCRateLimits(0, 999)
	if grpcRateLimit != 6000 {
		t.Fatalf("per-IP changed by a zero override: got %d, want 6000", grpcRateLimit)
	}
	if grpcPerPubkeyRateLimit != 999 {
		t.Fatalf("per-pubkey = %d, want 999", grpcPerPubkeyRateLimit)
	}
	SetGRPCRateLimits(-5, 0)
	if grpcRateLimit != 6000 {
		t.Fatalf("per-IP changed by a negative override: got %d, want 6000", grpcRateLimit)
	}
	if grpcPerPubkeyRateLimit != 999 {
		t.Fatalf("per-pubkey changed by a zero override: got %d, want 999", grpcPerPubkeyRateLimit)
	}
}

// --- per-IP interceptor smoke ---

// TestGRPCRateLimitInterceptor_PerIPThrottles verifies the pre-auth per-IP
// interceptor throttles a burst from one peer IP and that ResourceExhausted is
// returned (no trailer/server-directed delay is stamped).
func TestGRPCRateLimitInterceptor_PerIPThrottles(t *testing.T) {
	old := grpcRateLimit
	grpcRateLimit = 3
	defer func() { grpcRateLimit = old }()

	store := newRateLimitStore()
	interceptor := grpcRateLimitInterceptor(store, nil, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.volunteer.v1.VolunteerService/RequestWorkUnit"}

	for i := 0; i < grpcRateLimit; i++ {
		ctx := peerCtx("198.51.100.10:5555", "", "")
		var ran bool
		if _, err := interceptor(ctx, nil, info, okHandler(&ran)); err != nil {
			t.Fatalf("call %d: unexpected error %v", i+1, err)
		}
	}

	ctx := peerCtx("198.51.100.10:5555", "", "")
	var ran bool
	_, err := interceptor(ctx, nil, info, okHandler(&ran))
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over per-IP budget: want ResourceExhausted, got %v", err)
	}

	// A different IP retains an independent budget.
	ctx2 := peerCtx("198.51.100.99:5555", "", "")
	var ran2 bool
	if _, err := interceptor(ctx2, nil, info, okHandler(&ran2)); err != nil {
		t.Fatalf("distinct IP should not be throttled: got %v", err)
	}
}

// TestGRPCRateLimitInterceptor_TrustedProxyBucketsPerClient verifies that with a
// trusted proxy configured, two real clients arriving through the same proxy peer
// IP (distinguished only by x-forwarded-for) get independent per-IP buckets.
func TestGRPCRateLimitInterceptor_TrustedProxyBucketsPerClient(t *testing.T) {
	old := grpcRateLimit
	grpcRateLimit = 2
	defer func() { grpcRateLimit = old }()

	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	store := newRateLimitStore()
	interceptor := grpcRateLimitInterceptor(store, trusted, nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.volunteer.v1.VolunteerService/RequestWorkUnit"}

	clientA := func() error {
		ctx := peerCtx("10.0.0.1:1234", "203.0.113.7", "")
		var ran bool
		_, err := interceptor(ctx, nil, info, okHandler(&ran))
		return err
	}
	clientB := func() error {
		ctx := peerCtx("10.0.0.1:1234", "203.0.113.8", "")
		var ran bool
		_, err := interceptor(ctx, nil, info, okHandler(&ran))
		return err
	}

	// Exhaust client A's budget.
	for i := 0; i < grpcRateLimit; i++ {
		if err := clientA(); err != nil {
			t.Fatalf("clientA call %d: %v", i+1, err)
		}
	}
	if err := clientA(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("clientA over budget: want ResourceExhausted, got %v", err)
	}

	// Client B, same proxy peer but different real IP, must still pass.
	if err := clientB(); err != nil {
		t.Fatalf("clientB should have an independent bucket: got %v", err)
	}
}

// --- in-flight work-lifecycle exemption (TODO #32) ---

// TestIsRateLimitExempt verifies the trailing-method-name classification: the
// in-flight lifecycle RPCs are exempt; the discovery/registration/public methods
// are not. Independent of the proto package path prefix.
func TestIsRateLimitExempt(t *testing.T) {
	exempt := []string{"StartWork", "SubmitResult", "AbandonWorkUnit", "SaveCheckpoint", "GetCheckpoint"}
	for _, m := range exempt {
		if !isRateLimitExempt("/lettuce.v1.VolunteerService/" + m) {
			t.Errorf("%s should be exempt", m)
		}
		// Path-prefix independence.
		if !isRateLimitExempt("/some.other.pkg.Svc/" + m) {
			t.Errorf("%s should be exempt regardless of package path", m)
		}
	}
	limited := []string{"RegisterVolunteer", "RequestWorkUnit", "GetServerStatus", "GetHeadInfo"}
	for _, m := range limited {
		if isRateLimitExempt("/lettuce.v1.VolunteerService/" + m) {
			t.Errorf("%s must NOT be exempt", m)
		}
	}
}

// TestGRPCRateLimit_LifecycleMethodsExempt verifies that the in-flight lifecycle
// RPCs sail past BOTH limiters even when the budgets are tiny, while
// RequestWorkUnit on the same IP/pubkey is still throttled — i.e. a fast volunteer
// can always finish work it holds, but new-work discovery stays limited.
func TestGRPCRateLimit_LifecycleMethodsExempt(t *testing.T) {
	oldIP, oldPK := grpcRateLimit, grpcPerPubkeyRateLimit
	grpcRateLimit, grpcPerPubkeyRateLimit = 2, 2
	defer func() { grpcRateLimit, grpcPerPubkeyRateLimit = oldIP, oldPK }()

	// Per-IP interceptor: StartWork/SubmitResult from one IP, far past the limit.
	t.Run("per-IP exempt", func(t *testing.T) {
		store := newRateLimitStore()
		ic := grpcRateLimitInterceptor(store, nil, nil)
		for _, m := range []string{"StartWork", "SubmitResult", "AbandonWorkUnit", "SaveCheckpoint", "GetCheckpoint"} {
			info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.v1.VolunteerService/" + m}
			for i := 0; i < 10; i++ {
				ctx := peerCtx("198.51.100.10:5555", "", "")
				var ran bool
				if _, err := ic(ctx, nil, info, okHandler(&ran)); err != nil {
					t.Fatalf("%s call %d should be exempt from per-IP limit: got %v", m, i+1, err)
				}
			}
		}
		// New-work request from the SAME IP is still throttled past the budget.
		rwu := &grpc.UnaryServerInfo{FullMethod: "/lettuce.v1.VolunteerService/RequestWorkUnit"}
		for i := 0; i < grpcRateLimit; i++ {
			ctx := peerCtx("198.51.100.10:5555", "", "")
			var ran bool
			if _, err := ic(ctx, nil, rwu, okHandler(&ran)); err != nil {
				t.Fatalf("RequestWorkUnit call %d: unexpected error %v", i+1, err)
			}
		}
		ctx := peerCtx("198.51.100.10:5555", "", "")
		var ran bool
		if _, err := ic(ctx, nil, rwu, okHandler(&ran)); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("RequestWorkUnit over budget: want ResourceExhausted, got %v", err)
		}
	})

	// Per-pubkey interceptor: SubmitResult from one pubkey, far past the limit.
	t.Run("per-pubkey exempt", func(t *testing.T) {
		store := newRateLimitStore()
		ic := grpcPerPubkeyRateLimitInterceptor(store, nil)
		pk := newPubkey(t)
		for _, m := range []string{"StartWork", "SubmitResult", "AbandonWorkUnit", "SaveCheckpoint", "GetCheckpoint"} {
			info := &grpc.UnaryServerInfo{FullMethod: "/lettuce.v1.VolunteerService/" + m}
			for i := 0; i < 10; i++ {
				ctx := contextWithGRPCAuthPublicKey(context.Background(), pk)
				var ran bool
				if _, err := ic(ctx, nil, info, okHandler(&ran)); err != nil {
					t.Fatalf("%s call %d should be exempt from per-pubkey limit: got %v", m, i+1, err)
				}
			}
		}
		// Exempt methods must mint NO per-pubkey buckets.
		if len(store.buckets) != 0 {
			t.Fatalf("exempt methods should mint no per-pubkey buckets, got %d", len(store.buckets))
		}
		// RequestWorkUnit from the SAME pubkey is still throttled past the budget.
		rwu := &grpc.UnaryServerInfo{FullMethod: "/lettuce.v1.VolunteerService/RequestWorkUnit"}
		for i := 0; i < grpcPerPubkeyRateLimit; i++ {
			ctx := contextWithGRPCAuthPublicKey(context.Background(), pk)
			var ran bool
			if _, err := ic(ctx, nil, rwu, okHandler(&ran)); err != nil {
				t.Fatalf("RequestWorkUnit call %d: unexpected error %v", i+1, err)
			}
		}
		ctx := contextWithGRPCAuthPublicKey(context.Background(), pk)
		var ran bool
		if _, err := ic(ctx, nil, rwu, okHandler(&ran)); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("RequestWorkUnit over budget: want ResourceExhausted, got %v", err)
		}
	})
}
