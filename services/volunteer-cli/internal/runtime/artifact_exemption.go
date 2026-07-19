package runtime

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Local-testing opt-in for the artifact netguard (PB-6).
//
// Every leaf-artifact download (native binary, wasm module, external input,
// viz bundle) is normally screened by the netguard dial hook, which refuses
// loopback, private, link-local, and CGNAT addresses (BG-14). That is the right
// default, but it makes the documented local dry-run flow — a head and its
// artifact hosting on the operator's own machine or LAN — impossible for a
// released volunteer. This file is the explicit, loud, OFF-by-default escape
// hatch, mirroring the head's LETTUCE_BINARY_URL_ALLOW_INSECURE:
//
//   - The operator sets LETTUCE_VOLUNTEER_ALLOW_PRIVATE_ARTIFACTS to a
//     comma-separated list of head names (as configured / shown by `status`;
//     for an unnamed head this is its gRPC address) when starting the daemon.
//   - Only work units DISPATCHED BY a listed head may fetch artifacts from
//     private/loopback addresses; every other head's downloads keep the full
//     dial screen. There is no wildcard and no global toggle.
//   - Every download that uses the exemption is WARN-logged.
//   - Nothing else is relaxed: checksum verification, size caps, redirect
//     bounds, and timeouts are identical to the guarded path.
const AllowPrivateArtifactsEnv = "LETTUCE_VOLUNTEER_ALLOW_PRIVATE_ARTIFACTS"

// PrivateArtifactHeads returns the set of head names the operator listed in
// LETTUCE_VOLUNTEER_ALLOW_PRIVATE_ARTIFACTS (trimmed, empty entries dropped),
// or nil when the opt-in is unset. Parsed per call: it is consulted once per
// artifact download, and re-reading keeps the behavior testable and obvious.
func PrivateArtifactHeads() []string {
	raw := os.Getenv(AllowPrivateArtifactsEnv)
	if raw == "" {
		return nil
	}
	var heads []string
	for _, entry := range strings.Split(raw, ",") {
		if e := strings.TrimSpace(entry); e != "" {
			heads = append(heads, e)
		}
	}
	return heads
}

// PrivateArtifactsAllowedForHead reports whether the operator explicitly opted
// the named head into private/loopback artifact fetches. An empty head name
// never matches: a work unit whose source head is unknown always keeps the full
// dial screen.
func PrivateArtifactsAllowedForHead(head string) bool {
	if head == "" {
		return false
	}
	for _, h := range PrivateArtifactHeads() {
		if strings.EqualFold(h, head) {
			return true
		}
	}
	return false
}

// artifactClientForUnit returns the HTTP client for one work unit's artifact
// downloads: the runtime's guarded client, unless this unit's source head is
// explicitly opted in — in which case the use is WARN-logged and the unguarded
// (but otherwise identical) client is returned.
func artifactClientForUnit(guarded *http.Client, wu *WorkUnit, logger *slog.Logger) *http.Client {
	if !PrivateArtifactsAllowedForHead(wu.SourceHead) {
		return guarded
	}
	if logger != nil {
		logger.Warn("SECURITY: netguard dial screen DISABLED for this unit's artifact downloads — its head is listed in "+AllowPrivateArtifactsEnv,
			"head", wu.SourceHead, "work_unit_id", wu.ID)
	}
	return unguardedArtifactClient()
}

var (
	unguardedClientOnce sync.Once
	unguardedClient     *http.Client
)

// unguardedArtifactClient is NewGuardedHTTPClient without the netguard dial
// screen — every other property (no env proxy, bounded redirects, timeouts,
// connection limits) is kept identical so the opt-in relaxes exactly one thing.
func unguardedArtifactClient() *http.Client {
	unguardedClientOnce.Do(func() {
		unguardedClient = &http.Client{
			Timeout:       DefaultDownloadTimeout,
			CheckRedirect: boundedRedirect,
			Transport: &http.Transport{
				Proxy: nil, // never ProxyFromEnvironment; parity with the guarded client
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		}
	})
	return unguardedClient
}
