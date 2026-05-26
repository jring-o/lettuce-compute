package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

func TestRateLimitMiddleware_UnderLimitPassesThrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler, cleanup := rateLimitMiddleware(inner, nil)
	defer cleanup()

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Check rate limit headers are present.
	if w.Header().Get("X-RateLimit-Limit") == "" {
		t.Fatal("missing X-RateLimit-Limit header")
	}
	if w.Header().Get("X-RateLimit-Remaining") == "" {
		t.Fatal("missing X-RateLimit-Remaining header")
	}
	if w.Header().Get("X-RateLimit-Reset") == "" {
		t.Fatal("missing X-RateLimit-Reset header")
	}

	limit, _ := strconv.Atoi(w.Header().Get("X-RateLimit-Limit"))
	if limit != unauthenticatedRateLimit {
		t.Fatalf("expected limit %d for unauthenticated, got %d", unauthenticatedRateLimit, limit)
	}
}

func TestRateLimitMiddleware_ExceedsLimitReturns429(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler, cleanup := rateLimitMiddleware(inner, nil)
	defer cleanup()

	// Exhaust the unauthenticated limit (20 requests).
	for i := 0; i < unauthenticatedRateLimit; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// Next request should be rate limited.
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header on 429 response")
	}
}

func TestRateLimitMiddleware_AuthenticatedGetHigherLimit(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler, cleanup := rateLimitMiddleware(inner, nil)
	defer cleanup()

	user := &AuthUser{
		ID:   types.NewID(),
		Role: "USER",
	}

	// Make a request as an authenticated user.
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.2:12345"
	ctx := ContextWithUser(req.Context(), user)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	limit, _ := strconv.Atoi(w.Header().Get("X-RateLimit-Limit"))
	if limit != authenticatedRateLimit {
		t.Fatalf("expected limit %d for authenticated, got %d", authenticatedRateLimit, limit)
	}
}

func TestRateLimitMiddleware_DifferentIPsGetIndependentBuckets(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler, cleanup := rateLimitMiddleware(inner, nil)
	defer cleanup()

	// Exhaust IP1's limit.
	for i := 0; i < unauthenticatedRateLimit; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.1.1.1:12345"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// IP1 should be rate limited.
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.1.1.1:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("IP1 should be rate limited, got %d", w.Code)
	}

	// IP2 should still work.
	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.2.2.2:12345"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("IP2 should not be rate limited, got %d", w.Code)
	}
}

func TestRateLimitMiddleware_HeadersPresentOnAllResponses(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler, cleanup := rateLimitMiddleware(inner, nil)
	defer cleanup()

	requiredHeaders := []string{
		"X-RateLimit-Limit",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
	}

	// Check headers on a normal response.
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.3.3.3:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	for _, h := range requiredHeaders {
		if w.Header().Get(h) == "" {
			t.Fatalf("missing %s header on 200 response", h)
		}
	}

	// Exhaust limit and check headers on 429 response.
	for i := 0; i < unauthenticatedRateLimit; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "10.4.4.4:12345"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.4.4.4:12345"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	for _, h := range requiredHeaders {
		if w.Header().Get(h) == "" {
			t.Fatalf("missing %s header on 429 response", h)
		}
	}
}

// --- clientIP / clientIPFromRequest tests (trust-aware, H3) ---

// mustCIDR parses a CIDR or bare IP into *net.IPNet for tests.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n
	}
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("mustCIDR: invalid %q", s)
	}
	mask := net.CIDRMask(32, 32)
	if ip.To4() == nil {
		mask = net.CIDRMask(128, 128)
	}
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}
}

// TestClientIP_NoTrust verifies that clientIP (which trusts no proxies) and
// clientIPFromRequest with an empty trusted list ALWAYS return the direct peer
// IP and never honor spoofable forwarding headers.
func TestClientIP_NoTrust(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		wantIP     string
	}{
		{
			name:       "RemoteAddr with port",
			remoteAddr: "192.168.1.1:12345",
			wantIP:     "192.168.1.1",
		},
		{
			name:       "RemoteAddr without port",
			remoteAddr: "192.168.1.1",
			wantIP:     "192.168.1.1",
		},
		{
			name:       "X-Forwarded-For is IGNORED (untrusted peer)",
			remoteAddr: "10.0.0.1:1234",
			xff:        "203.0.113.50",
			wantIP:     "10.0.0.1",
		},
		{
			name:       "X-Forwarded-For chain is IGNORED",
			remoteAddr: "10.0.0.1:1234",
			xff:        "203.0.113.50, 70.41.3.18, 150.172.238.178",
			wantIP:     "10.0.0.1",
		},
		{
			name:       "X-Real-IP is IGNORED",
			remoteAddr: "10.0.0.1:1234",
			xRealIP:    "198.51.100.1",
			wantIP:     "10.0.0.1",
		},
		{
			name:       "both headers IGNORED",
			remoteAddr: "10.0.0.1:1234",
			xff:        "203.0.113.50",
			xRealIP:    "198.51.100.1",
			wantIP:     "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xRealIP != "" {
				req.Header.Set("X-Real-IP", tt.xRealIP)
			}

			if got := clientIP(req); got != tt.wantIP {
				t.Fatalf("clientIP() = %q, want %q", got, tt.wantIP)
			}
			if got := clientIPFromRequest(req, nil); got != tt.wantIP {
				t.Fatalf("clientIPFromRequest(nil) = %q, want %q", got, tt.wantIP)
			}
		})
	}
}

// TestClientIPFromRequest_TrustedPeer verifies trust-aware extraction when the
// direct peer IS a configured trusted proxy: walk X-Forwarded-For right-to-left,
// skipping trusted hops, returning the right-most non-trusted entry.
func TestClientIPFromRequest_TrustedPeer(t *testing.T) {
	trusted := []*net.IPNet{
		mustCIDR(t, "10.0.0.0/8"),
		mustCIDR(t, "172.16.0.0/12"),
	}

	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		wantIP     string
	}{
		{
			name:       "trusted peer, single client in XFF",
			remoteAddr: "10.0.0.1:1234",
			xff:        "203.0.113.50",
			wantIP:     "203.0.113.50",
		},
		{
			name:       "trusted peer, right-most non-trusted entry wins",
			remoteAddr: "10.0.0.1:1234",
			// client, attacker-injected? no — right-to-left: 10.0.0.9 trusted (skip),
			// 70.41.3.18 is the real right-most client.
			xff:    "203.0.113.50, 70.41.3.18, 10.0.0.9",
			wantIP: "70.41.3.18",
		},
		{
			name:       "trusted peer, multiple trusted hops skipped",
			remoteAddr: "10.0.0.1:1234",
			xff:        "203.0.113.50, 172.16.5.5, 10.0.0.9",
			wantIP:     "203.0.113.50",
		},
		{
			name:       "trusted peer, spoofed left entry does NOT win",
			remoteAddr: "10.0.0.1:1234",
			// Attacker prepends a fake left-most IP; right-most real client is 70.41.3.18.
			xff:    "1.2.3.4, 70.41.3.18, 10.0.0.9",
			wantIP: "70.41.3.18",
		},
		{
			name:       "trusted peer, all XFF trusted → fall back to X-Real-IP",
			remoteAddr: "10.0.0.1:1234",
			xff:        "10.0.0.9, 172.16.1.1",
			xRealIP:    "198.51.100.7",
			wantIP:     "198.51.100.7",
		},
		{
			name:       "trusted peer, no XFF → X-Real-IP",
			remoteAddr: "10.0.0.1:1234",
			xRealIP:    "198.51.100.7",
			wantIP:     "198.51.100.7",
		},
		{
			name:       "trusted peer, no usable headers → peer IP",
			remoteAddr: "10.0.0.1:1234",
			wantIP:     "10.0.0.1",
		},
		{
			name:       "trusted peer, invalid XFF entries skipped",
			remoteAddr: "10.0.0.1:1234",
			xff:        "not-an-ip, 203.0.113.50, garbage",
			wantIP:     "203.0.113.50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xRealIP != "" {
				req.Header.Set("X-Real-IP", tt.xRealIP)
			}
			if got := clientIPFromRequest(req, trusted); got != tt.wantIP {
				t.Fatalf("clientIPFromRequest() = %q, want %q", got, tt.wantIP)
			}
		})
	}
}

// TestClientIPFromRequest_UntrustedPeerIgnoresHeaders verifies that when the
// direct peer is NOT in the trusted set, forwarding headers are ignored even
// though some trusted proxies are configured.
func TestClientIPFromRequest_UntrustedPeerIgnoresHeaders(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "203.0.113.99:5555" // public, NOT trusted
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "5.6.7.8")

	if got := clientIPFromRequest(req, trusted); got != "203.0.113.99" {
		t.Fatalf("untrusted peer should ignore headers: got %q, want %q", got, "203.0.113.99")
	}
}

// TestRateLimitMiddleware_SpoofedXFFDoesNotResetBucket verifies the core H3
// fix: with NO trusted proxies configured, rotating X-Forwarded-For does NOT
// mint a fresh token bucket — the limiter keys on the real peer IP, so the
// attacker is still throttled after the limit is exhausted.
func TestRateLimitMiddleware_SpoofedXFFDoesNotResetBucket(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler, cleanup := rateLimitMiddleware(inner, nil) // no trusted proxies
	defer cleanup()

	const peer = "198.51.100.42:9999"

	// Exhaust the unauthenticated limit, rotating XFF on every request.
	for i := 0; i < unauthenticatedRateLimit; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = peer
		req.Header.Set("X-Forwarded-For", "10.0.0."+strconv.Itoa(i)) // spoofed, rotating
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// One more request with yet another spoofed XFF: must STILL be limited
	// because the bucket is keyed on the real peer IP, not the header.
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = peer
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF should not reset bucket: expected 429, got %d", w.Code)
	}
}
