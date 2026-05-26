//go:build integration

package e2e_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/server"
)

// C1 added Ed25519 request authentication to every non-public gRPC method: the
// client must sign each request with the volunteer's private key and the server
// verifies authedKey == the acted-on volunteer. The e2e harnesses previously
// discarded the volunteer private key (`pub := genVolunteerKey(t)`),
// so they could not sign. These helpers keep the private key, register it in a
// pubkey→privkey map, and let call sites sign by wrapping the context with the
// volunteer's keys via server.ContextWithTestSigner (consumed by the client
// interceptor server.TestSigningInterceptor, installed on each test gRPC client).

// init relaxes the server's anti-replay and per-IP gRPC rate limiting for this
// integration test binary only (these guard against production abuse but conflict
// with the e2e harness's burst of byte-identical loopback RPCs). Production is
// unaffected; the seam lives in an integration-build-only file.
func init() {
	server.SetGRPCSecurityForIntegrationTests()
}

var e2eSignerKeys sync.Map // string(pubKey) -> ed25519.PrivateKey

// genVolunteerKey generates a real Ed25519 keypair, records the private half keyed
// by the public half, and returns the public key (used as the volunteer identity).
// Replaces the old `pub := genVolunteerKey(t)` which dropped the key.
func genVolunteerKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate volunteer key: %v", err)
	}
	e2eSignerKeys.Store(string(pub), priv)
	return pub
}

// signFor returns a context that signs the outgoing RPC with the keypair previously
// generated for pubKey. The client signing interceptor reads these keys and attaches
// a valid Ed25519 signature so the server's auth interceptor accepts the request.
func signFor(t *testing.T, ctx context.Context, pubKey []byte) context.Context {
	t.Helper()
	v, ok := e2eSignerKeys.Load(string(pubKey))
	if !ok {
		t.Fatalf("no signing key registered for public key %x (use genVolunteerKey)", pubKey)
	}
	priv := v.(ed25519.PrivateKey)
	return server.ContextWithTestSigner(ctx, ed25519.PublicKey(pubKey), priv)
}
