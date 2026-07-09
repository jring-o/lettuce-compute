package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// headHostUnknownRefusal is the head's FailedPrecondition host-unknown work-path
// refusal. Its text contains "outdated" on purpose (so pre-issuance builds classify it
// as too-old and print the update hint) — which is exactly why the fetcher must check
// IsHostUnknownError BEFORE IsVolunteerTooOldError. These tests pin that routing order.
func headHostUnknownRefusal() error {
	return status.Error(codes.FailedPrecondition,
		"unknown or revoked host id: this volunteer build is outdated — run 'lettuce-volunteer update' (updated builds re-register and acquire a fresh id automatically)")
}

func newReRegTestFetcher(t *testing.T, head *ServerConnection) *Fetcher {
	t.Helper()
	d := newFetcherTestDaemon([]*ServerConnection{head})
	queue := NewPreFetchQueue(8, d.logger)
	return NewFetcher(d, queue, d.weightedSelector, d.leafCache)
}

var reRegTestLeaf = CachedLeafInfo{ID: "leaf-1", Slug: "leaf-1", Name: "Leaf One", State: "ACTIVE"}

// F-G routing regression: a host-unknown refusal routes to re-register (discard id +
// re-register), NOT to the too-old/update-hint path — even though the same message ALSO
// matches IsVolunteerTooOldError. On a successful re-register the head keeps its fresh
// id, stays available, and grows no backoff.
func TestFetcher_HostUnknownRefusal_RoutesToReRegister(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, headHostUnknownRefusal()
		},
	}
	head := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "stale-id"}
	fetcher := newReRegTestFetcher(t, head)

	calls := 0
	fetcher.reRegisterFn = func(_ context.Context, h *ServerConnection) (string, error) {
		calls++
		if h != head {
			t.Errorf("re-register called for the wrong head: %v", h.Name)
		}
		return "fresh-id", nil
	}

	pushed, stop := fetcher.requestAndBuffer(context.Background(), head, reRegTestLeaf, []string{"leaf-1"}, nil)

	if calls != 1 {
		t.Fatalf("reRegisterFn calls = %d, want 1 (host-unknown must route to re-register, not the too-old path)", calls)
	}
	if pushed != 0 {
		t.Errorf("pushed = %d, want 0", pushed)
	}
	if !stop {
		t.Error("stop = false, want true (stop trying this head's leafs this cycle)")
	}
	if head.HostID != "fresh-id" {
		t.Errorf("head.HostID = %q, want fresh-id (adopted the re-registered id)", head.HostID)
	}
	if !head.Available {
		t.Error("head.Available = false; a successful re-register must keep the head available for immediate retry")
	}
	if head.Backoff != 0 {
		t.Errorf("head.Backoff = %v, want 0 (no backoff growth on successful re-register)", head.Backoff)
	}
}

// The head may re-register the machine into host-less mode (empty id: at-cap or the
// account has no slot). The fetcher adopts the empty id and keeps working host-less.
func TestFetcher_HostUnknownRefusal_AdoptsEmptyReRegister(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, headHostUnknownRefusal()
		},
	}
	head := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "stale-id"}
	fetcher := newReRegTestFetcher(t, head)
	fetcher.reRegisterFn = func(_ context.Context, _ *ServerConnection) (string, error) { return "", nil }

	_, stop := fetcher.requestAndBuffer(context.Background(), head, reRegTestLeaf, []string{"leaf-1"}, nil)
	if !stop {
		t.Error("stop = false, want true")
	}
	if head.HostID != "" {
		t.Errorf("head.HostID = %q, want empty (adopted host-less re-register)", head.HostID)
	}
	if !head.Available {
		t.Error("head should stay available after a successful (empty) re-register")
	}
}

// If the re-register itself fails (head transiently down), the fetcher keeps the stale
// id, marks the head unavailable, and grows backoff — the normal reconnect path, so it
// re-attempts on a later refusal.
func TestFetcher_HostUnknownRefusal_ReRegisterFailureBacksOff(t *testing.T) {
	mc := &mockClient{
		requestWorkUnitFn: func(_ context.Context, _ *lettucev1.RequestWorkUnitRequest) (*lettucev1.RequestWorkUnitResponse, error) {
			return nil, headHostUnknownRefusal()
		},
	}
	head := &ServerConnection{Client: mc, VolunteerID: "vol-1", Name: "server-a", Available: true, HostID: "stale-id"}
	fetcher := newReRegTestFetcher(t, head)
	fetcher.reRegisterFn = func(_ context.Context, _ *ServerConnection) (string, error) {
		return "", fmt.Errorf("head down")
	}

	_, stop := fetcher.requestAndBuffer(context.Background(), head, reRegTestLeaf, []string{"leaf-1"}, nil)
	if !stop {
		t.Error("stop = false, want true")
	}
	if head.Available {
		t.Error("head.Available = true; a failed re-register must mark the head unavailable")
	}
	if head.Backoff == 0 {
		t.Error("head.Backoff = 0; a failed re-register must grow the reconnect backoff")
	}
	if head.HostID != "stale-id" {
		t.Errorf("head.HostID = %q, want stale-id unchanged (no fresh id was acquired)", head.HostID)
	}
}

// reRegMockClient satisfies both WorkClient (via the embedded mockClient) and the
// narrow registerClient interface reRegisterHost type-asserts to.
type reRegMockClient struct {
	*mockClient
	lastReq *lettucev1.RegisterVolunteerRequest
	resp    *lettucev1.RegisterVolunteerResponse
	err     error
}

func (m *reRegMockClient) RegisterVolunteer(_ context.Context, req *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	m.lastReq = req
	return m.resp, m.err
}

// reRegisterHost sends an EMPTY host id (discard the refused id => the head mints one),
// adopts the returned id, and persists it to the store keyed by the head's gRPC address.
func TestDaemon_ReRegisterHost_MintsAndPersists(t *testing.T) {
	store := identity.NewHostIDStore(filepath.Join(t.TempDir(), "host-ids.json"))
	if err := store.Set("head-a:443", "stale"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rc := &reRegMockClient{
		mockClient: &mockClient{},
		resp:       &lettucev1.RegisterVolunteerResponse{VolunteerId: "v", Registered: false, HostId: "fresh"},
	}
	head := &ServerConnection{Client: rc, Name: "server-a", Config: config.ServerConfig{GRPCAddress: "head-a:443"}, HostID: "stale"}
	d := newFetcherTestDaemon([]*ServerConnection{head})
	d.hostIDStore = store

	id, err := d.reRegisterHost(context.Background(), head)
	if err != nil {
		t.Fatalf("reRegisterHost: %v", err)
	}
	if id != "fresh" {
		t.Errorf("returned id = %q, want fresh", id)
	}
	if rc.lastReq == nil || rc.lastReq.HostId != "" {
		t.Errorf("re-register request HostId = %q, want empty (mint request)", rc.lastReq.GetHostId())
	}
	if got, _ := store.Get("head-a:443"); got != "fresh" {
		t.Errorf("persisted id = %q, want fresh", got)
	}
}

// An empty re-register response (at-cap / no slot) deletes the stored id: the machine
// runs host-less until a later register frees a slot.
func TestDaemon_ReRegisterHost_EmptyDeletesStored(t *testing.T) {
	store := identity.NewHostIDStore(filepath.Join(t.TempDir(), "host-ids.json"))
	if err := store.Set("head-a:443", "stale"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	rc := &reRegMockClient{
		mockClient: &mockClient{},
		resp:       &lettucev1.RegisterVolunteerResponse{VolunteerId: "v", HostId: ""},
	}
	head := &ServerConnection{Client: rc, Name: "server-a", Config: config.ServerConfig{GRPCAddress: "head-a:443"}, HostID: "stale"}
	d := newFetcherTestDaemon([]*ServerConnection{head})
	d.hostIDStore = store

	id, err := d.reRegisterHost(context.Background(), head)
	if err != nil {
		t.Fatalf("reRegisterHost: %v", err)
	}
	if id != "" {
		t.Errorf("returned id = %q, want empty", id)
	}
	if got, _ := store.Get("head-a:443"); got != "" {
		t.Errorf("stored id = %q, want empty (deleted)", got)
	}
}
