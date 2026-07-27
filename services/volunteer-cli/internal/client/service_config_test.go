package client

import (
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc/resolver"
)

// recordingResolverBuilder is a resolver for a private scheme that records the
// BuildOptions gRPC hands it. DisableServiceConfig is the flag we care about: gRPC sets
// it from grpc.WithDisableServiceConfig(), and its DNS resolver consults exactly that
// flag to decide whether to look up the "_grpc_config.<host>" TXT record.
type recordingResolverBuilder struct {
	built chan resolver.BuildOptions
}

func (b *recordingResolverBuilder) Build(_ resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	select {
	case b.built <- opts:
	default:
	}
	// Report one bogus address so the ClientConn has something to work with; the test
	// never sends an RPC, so nothing dials it.
	_ = cc.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: "127.0.0.1:1"}}})
	return &noopResolver{}, nil
}

func (b *recordingResolverBuilder) Scheme() string { return "lettucetestres" }

type noopResolver struct{}

func (*noopResolver) ResolveNow(resolver.ResolveNowOptions) {}
func (*noopResolver) Close()                                {}

// TestNewDisablesResolverServiceConfig asserts the client tells the resolver NOT to fetch
// a service config from DNS.
//
// This is the whole of the fix, so it is worth being explicit about what it buys. gRPC's
// DNS resolver, when this flag is false, looks up a TXT record at "_grpc_config." + the
// head's hostname on every resolution. Heads do not publish that record, and a network
// whose DNS never answers a negative lookup (measured at ~11s against two independent
// testers' networks) stalls the whole connection there — longer than the client's own 10s
// RPC deadline, which is why every daemon start failed its first connection and succeeded
// on the retry, and why doctor reported a healthy head unreachable.
//
// The assertion is on the flag rather than on wall-clock timing on purpose: a timing test
// would need a DNS server that black-holes negative queries to fail, so it would pass on
// CI whether or not the fix were present, which is worse than no test at all.
func TestNewDisablesResolverServiceConfig(t *testing.T) {
	b := &recordingResolverBuilder{built: make(chan resolver.BuildOptions, 1)}
	resolver.Register(b)

	c, err := New(ClientConfig{ServerURL: b.Scheme() + ":///head.example", Insecure: true}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// The connection is lazy; nudge it so gRPC builds the resolver.
	c.conn.Connect()

	select {
	case opts := <-b.built:
		if !opts.DisableServiceConfig {
			t.Fatal("resolver was built with DisableServiceConfig=false: the client will look up " +
				"a _grpc_config TXT record for every head, which stalls the first connection " +
				"past its deadline on any network that does not promptly answer a negative DNS query")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolver was never built; cannot tell whether service config is disabled")
	}
}
