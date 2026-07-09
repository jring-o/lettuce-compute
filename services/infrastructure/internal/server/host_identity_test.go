package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- test fakes ---------------------------------------------------------------

// stubHostRepo is a fully-implemented volunteer.HostRepository whose behavior each test
// wires via the func fields. It records call counts and the arguments the mint path threads
// (the cap policy) so the register/work paths can be driven without a database. (The
// dispatch-cache package already has fakeHostRepo, but that one implements only GetByID.)
type stubHostRepo struct {
	mu sync.Mutex

	getByIDFn func(id types.ID) (*volunteer.Host, error)
	mintFn    func(h *volunteer.Host) (bool, error)
	upsertErr error

	getByIDCalls        int
	mintCalls           int
	upsertCalls         int
	updateLastSeenCalls int
	lastMintCap         int
	lastMintWindow      time.Duration
}

func (s *stubHostRepo) Mint(_ context.Context, h *volunteer.Host, capPerAccount int, activeWindow time.Duration) (bool, error) {
	s.mu.Lock()
	s.mintCalls++
	s.lastMintCap = capPerAccount
	s.lastMintWindow = activeWindow
	fn := s.mintFn
	s.mu.Unlock()
	if fn != nil {
		return fn(h)
	}
	return true, nil
}

func (s *stubHostRepo) Upsert(_ context.Context, _ *volunteer.Host) error {
	s.mu.Lock()
	s.upsertCalls++
	err := s.upsertErr
	s.mu.Unlock()
	return err
}

func (s *stubHostRepo) GetByID(_ context.Context, id types.ID) (*volunteer.Host, error) {
	s.mu.Lock()
	s.getByIDCalls++
	fn := s.getByIDFn
	s.mu.Unlock()
	if fn != nil {
		return fn(id)
	}
	return nil, apierror.NotFound("host", id.String())
}

func (s *stubHostRepo) UpdateLastSeen(_ context.Context, _ types.ID) error {
	s.mu.Lock()
	s.updateLastSeenCalls++
	s.mu.Unlock()
	return nil
}

// newHostIdentityService builds a *volunteerService wired for the resolveRegisteredHost
// tests: the host repo, the cap policy, and a (possibly nil) dispatch cache for warming.
func newHostIdentityService(hostRepo volunteer.HostRepository, cache *dispatchCache, cap HostCapPolicy) *volunteerService {
	return &volunteerService{
		hostRepo:      hostRepo,
		hostCap:       cap,
		dispatchCache: cache,
		logger:        testLogger(),
		now:           time.Now,
	}
}

// --- (a) refusal-message contract ---------------------------------------------

// TestHostUnknownMessage_Contract pins the two-audience wording contract of the host-unknown
// refusal (mirroring internal/admission's TestPowRequiredMessage_Contract). Issuance-era
// clients match HostUnknownMessagePrefix to trigger discard-id-and-re-register; pre-issuance
// builds (which echo self-generated ids the head never issued) classify the same status via
// IsVolunteerTooOldError, which fires on "too old"/"outdated", and print the update hint.
// Changing either constant silently orphans one audience, so both are pinned.
func TestHostUnknownMessage_Contract(t *testing.T) {
	// (a) The full message must carry the machine-readable prefix issuance-era clients match.
	if !strings.HasPrefix(HostUnknownMessage, HostUnknownMessagePrefix) {
		t.Errorf("HostUnknownMessage %q does not start with HostUnknownMessagePrefix %q",
			HostUnknownMessage, HostUnknownMessagePrefix)
	}

	// (b) The message must trigger the pre-issuance too-old classifier (contains "too old"
	// or "outdated"). It uses "outdated".
	msg := strings.ToLower(HostUnknownMessage)
	if !strings.Contains(msg, "too old") && !strings.Contains(msg, "outdated") {
		t.Errorf("HostUnknownMessage %q contains neither \"too old\" nor \"outdated\"; "+
			"pre-issuance builds would show a generic error instead of the update hint",
			HostUnknownMessage)
	}

	// (c) The prefix is the shipped machine contract: exact bytes, and already lower-case
	// (issuance-era clients lower-case nothing before matching it).
	const wantPrefix = "unknown or revoked host id"
	if HostUnknownMessagePrefix != wantPrefix {
		t.Errorf("HostUnknownMessagePrefix = %q, want %q (changing it orphans shipped clients)",
			HostUnknownMessagePrefix, wantPrefix)
	}
	if HostUnknownMessagePrefix != strings.ToLower(HostUnknownMessagePrefix) {
		t.Errorf("HostUnknownMessagePrefix %q is not lower-case stable", HostUnknownMessagePrefix)
	}
}

// --- (b) host-owner cache -----------------------------------------------------

// TestPutThenResolveHostOwner_MemoryHit: a warmed ownership fact resolves entirely in memory,
// with NO host-repo DB touch (the hot-path property warmed at registration).
func TestPutThenResolveHostOwner_MemoryHit(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	hr := &fakeHostRepo{hosts: map[types.ID]*volunteer.Host{}}
	c.deps.hostRepo = hr

	host := types.NewID()
	owner := types.NewID()
	c.putHostOwner(host, owner)

	got, found, definitive := c.resolveHostOwner(host)
	if !found || !definitive || got != owner {
		t.Fatalf("warmed resolve = (%v, %v, %v), want (%v, true, true)", got, found, definitive, owner)
	}
	if hr.calls() != 0 {
		t.Errorf("a warmed owner resolve must not read the DB, got %d calls", hr.calls())
	}
}

// TestResolveHostOwner_ColdMissPositiveThenCached: a cold miss reads the hosts row ONCE and
// caches the positive outcome, so a second resolve is served from memory.
func TestResolveHostOwner_ColdMissPositiveThenCached(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	host := types.NewID()
	owner := types.NewID()
	c.deps.hostRepo = &fakeHostRepo{hosts: map[types.ID]*volunteer.Host{
		host: {ID: host, VolunteerID: owner},
	}}

	if got, found, definitive := c.resolveHostOwner(host); !found || !definitive || got != owner {
		t.Fatalf("cold-miss positive resolve = (%v, %v, %v), want (%v, true, true)", got, found, definitive, owner)
	}
	if got, found, definitive := c.resolveHostOwner(host); !found || !definitive || got != owner {
		t.Fatalf("second resolve = (%v, %v, %v), want the cached (%v, true, true)", got, found, definitive, owner)
	}
	if hr, _ := c.deps.hostRepo.(*fakeHostRepo); hr.calls() != 1 {
		t.Errorf("a positive outcome should be cached after ONE read, got %d", hr.calls())
	}
}

// TestResolveHostOwner_NegativeCached: a definitive not-found is cached for the TTL, so a
// client hammering an unknown id costs at most one DB read (audit F-F).
func TestResolveHostOwner_NegativeCached(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	hr := &fakeHostRepo{hosts: map[types.ID]*volunteer.Host{}} // every id is unknown
	c.deps.hostRepo = hr

	unknown := types.NewID()
	if _, found, definitive := c.resolveHostOwner(unknown); found || !definitive {
		t.Fatalf("unknown id resolve = (found=%v, definitive=%v), want (false, true)", found, definitive)
	}
	// A second lookup of the same unknown id is served from the NEGATIVE cache entry.
	if _, found, definitive := c.resolveHostOwner(unknown); found || !definitive {
		t.Fatalf("second unknown resolve = (found=%v, definitive=%v), want the cached (false, true)", found, definitive)
	}
	if hr.calls() != 1 {
		t.Errorf("a negative outcome should be cached (one DB read per id per TTL), got %d reads", hr.calls())
	}
}

// TestResolveHostOwner_NoRepoUndeterminable: with no host repo the outcome is undeterminable
// (definitive=false), so the caller folds to the account bucket rather than refusing.
func TestResolveHostOwner_NoRepoUndeterminable(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	c.deps.hostRepo = nil

	owner, found, definitive := c.resolveHostOwner(types.NewID())
	if found || definitive || !types.IsNilID(owner) {
		t.Fatalf("no-repo resolve = (%v, %v, %v), want (nil, false, false) — the caller folds, never refuses",
			owner, found, definitive)
	}
}

// TestResolveHostOwner_ShedUndeterminable: a saturated dispatch-admission semaphore makes a
// cold-miss owner read undeterminable (definitive=false) — the caller folds, never refuses.
func TestResolveHostOwner_ShedUndeterminable(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	c.deps.hostRepo = &fakeHostRepo{hosts: map[types.ID]*volunteer.Host{}}
	// Saturate the admission semaphore so the cold-miss read cannot be admitted.
	for i := 0; i < cap(c.admission); i++ {
		c.admission <- struct{}{}
	}
	if _, found, definitive := c.resolveHostOwner(types.NewID()); found || definitive {
		t.Fatalf("saturated admission should fold (definitive=false), not refuse (found=%v definitive=%v)", found, definitive)
	}
}

// TestResolveHostOwner_ExpiredEntryNotServed: an expired cache entry is not served — with no
// repo behind it the resolve falls through to the fold path, proving the stale entry was
// discarded (this is the ≤ TTL revocation latency by construction).
func TestResolveHostOwner_ExpiredEntryNotServed(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	c.deps.hostRepo = nil // an expired entry can then only fold, proving it was not served
	host := types.NewID()
	owner := types.NewID()
	c.putHostOwner(host, owner)

	// Force the cached entry to be expired.
	c.hostOwnerMu.Lock()
	c.hostOwnerCache[host].expires = time.Now().Add(-time.Second)
	c.hostOwnerMu.Unlock()

	// A fresh entry would return the owner; an expired one falls through to the fold path.
	if got, found, definitive := c.resolveHostOwner(host); found || definitive || !types.IsNilID(got) {
		t.Fatalf("expired entry must not be served: got (%v, %v, %v), want (nil, false, false)", got, found, definitive)
	}
}

// TestShouldBumpHostSeen_ThrottleWindow: the first bump check after warming is due; a second
// check within the throttle interval is not (true-then-false-within-interval).
func TestShouldBumpHostSeen_ThrottleWindow(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	host := types.NewID()
	c.putHostOwner(host, types.NewID())

	if !c.shouldBumpHostSeen(host) {
		t.Fatal("first shouldBumpHostSeen after warming should be true (no prior bump)")
	}
	if c.shouldBumpHostSeen(host) {
		t.Fatal("second shouldBumpHostSeen within the throttle interval should be false")
	}
}

// TestShouldBumpHostSeen_NoEntryCreatesAndBumps: with no cache entry at all (e.g. the warmed
// entry expired between validation and the bump check), the first check creates a
// bump-tracking entry and reports due, then throttles.
func TestShouldBumpHostSeen_NoEntryCreatesAndBumps(t *testing.T) {
	c := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	host := types.NewID()

	if !c.shouldBumpHostSeen(host) {
		t.Fatal("shouldBumpHostSeen with no entry should be true and create the entry")
	}
	if c.shouldBumpHostSeen(host) {
		t.Fatal("second check within the interval should be false")
	}
}

// --- (c) resolveRegisteredHost three-way --------------------------------------

func hostRegisterReq(hostID string) *lettucev1.RegisterVolunteerRequest {
	return &lettucev1.RegisterVolunteerRequest{
		HostId:            hostID,
		AvailableRuntimes: []string{leaf.RuntimeNative},
	}
}

// TestResolveRegisteredHost_EchoOwnedRefreshes: a request echoing a known id of THIS account
// refreshes the row (Upsert), echoes the same id back, and warms the owner cache; it never
// mints.
func TestResolveRegisteredHost_EchoOwnedRefreshes(t *testing.T) {
	vol := types.NewID()
	host := types.NewID()
	stub := &stubHostRepo{
		getByIDFn: func(id types.ID) (*volunteer.Host, error) {
			return &volunteer.Host{ID: id, VolunteerID: vol}, nil
		},
	}
	cache := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	svc := newHostIdentityService(stub, cache, HostCapPolicy{})

	got := svc.resolveRegisteredHost(context.Background(), vol, hostRegisterReq(host.String()), volunteer.HardwareCapabilities{}, time.Now())
	if got != host.String() {
		t.Fatalf("echo of an owned id = %q, want %q", got, host.String())
	}
	if stub.upsertCalls != 1 {
		t.Errorf("echo path should refresh the row via Upsert once, got %d", stub.upsertCalls)
	}
	if stub.mintCalls != 0 {
		t.Errorf("echo path must not mint, got %d mint calls", stub.mintCalls)
	}
	if owner, found, definitive := cache.resolveHostOwner(host); !found || !definitive || owner != vol {
		t.Errorf("echo should warm the owner cache, got (%v, %v, %v)", owner, found, definitive)
	}
}

// TestResolveRegisteredHost_UnknownForeignUnparseableReturnEmpty: an unknown, foreign, or
// unparseable echoed id yields an empty response id and creates NOTHING (mint-on-unknown is
// deliberately not implemented — the client re-registers empty to mint).
func TestResolveRegisteredHost_UnknownForeignUnparseableReturnEmpty(t *testing.T) {
	vol := types.NewID()

	t.Run("unknown", func(t *testing.T) {
		stub := &stubHostRepo{
			getByIDFn: func(id types.ID) (*volunteer.Host, error) {
				return nil, apierror.NotFound("host", id.String())
			},
		}
		svc := newHostIdentityService(stub, nil, HostCapPolicy{})
		if got := svc.resolveRegisteredHost(context.Background(), vol, hostRegisterReq(types.NewID().String()), volunteer.HardwareCapabilities{}, time.Now()); got != "" {
			t.Errorf("unknown echo = %q, want empty", got)
		}
		if stub.mintCalls != 0 || stub.upsertCalls != 0 {
			t.Errorf("unknown echo must create nothing (mint=%d upsert=%d)", stub.mintCalls, stub.upsertCalls)
		}
	})

	t.Run("foreign", func(t *testing.T) {
		other := types.NewID()
		host := types.NewID()
		stub := &stubHostRepo{
			getByIDFn: func(id types.ID) (*volunteer.Host, error) {
				return &volunteer.Host{ID: id, VolunteerID: other}, nil // owned by another account
			},
		}
		svc := newHostIdentityService(stub, nil, HostCapPolicy{})
		if got := svc.resolveRegisteredHost(context.Background(), vol, hostRegisterReq(host.String()), volunteer.HardwareCapabilities{}, time.Now()); got != "" {
			t.Errorf("foreign echo = %q, want empty", got)
		}
		if stub.upsertCalls != 0 {
			t.Errorf("foreign echo must not refresh the row, got %d upsert calls", stub.upsertCalls)
		}
	})

	t.Run("unparseable", func(t *testing.T) {
		stub := &stubHostRepo{}
		svc := newHostIdentityService(stub, nil, HostCapPolicy{})
		if got := svc.resolveRegisteredHost(context.Background(), vol, hostRegisterReq("not-a-uuid"), volunteer.HardwareCapabilities{}, time.Now()); got != "" {
			t.Errorf("unparseable echo = %q, want empty", got)
		}
		if stub.mintCalls != 0 || stub.upsertCalls != 0 || stub.getByIDCalls != 0 {
			t.Errorf("unparseable echo must not touch the repo at all (mint=%d upsert=%d get=%d)",
				stub.mintCalls, stub.upsertCalls, stub.getByIDCalls)
		}
	})
}

// TestResolveRegisteredHost_EmptyMints: an explicitly empty id is a mint request — a fresh id
// is minted under the cap, returned, and the owner cache is warmed; the cap policy is threaded
// into Mint verbatim.
func TestResolveRegisteredHost_EmptyMints(t *testing.T) {
	vol := types.NewID()
	stub := &stubHostRepo{mintFn: func(_ *volunteer.Host) (bool, error) { return true, nil }}
	cache := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	svc := newHostIdentityService(stub, cache, HostCapPolicy{PerAccount: 10, ActiveWindow: hostActiveWindowUnit})

	got := svc.resolveRegisteredHost(context.Background(), vol, hostRegisterReq(""), volunteer.HardwareCapabilities{}, time.Now())
	if got == "" {
		t.Fatal("an empty host id should mint and return a fresh id, got empty")
	}
	mintedID, err := types.ParseID(got)
	if err != nil {
		t.Fatalf("minted host id %q is not a valid id: %v", got, err)
	}
	if stub.mintCalls != 1 {
		t.Errorf("empty path should mint once, got %d", stub.mintCalls)
	}
	if stub.lastMintCap != 10 || stub.lastMintWindow != hostActiveWindowUnit {
		t.Errorf("Mint got cap=%d window=%v, want 10 / %v (cap policy threaded verbatim)",
			stub.lastMintCap, stub.lastMintWindow, hostActiveWindowUnit)
	}
	if owner, found, definitive := cache.resolveHostOwner(mintedID); !found || !definitive || owner != vol {
		t.Errorf("mint should warm the owner cache for the new id, got (%v, %v, %v)", owner, found, definitive)
	}
}

// TestResolveRegisteredHost_CapRefusalReturnsEmpty: a mint refused by the cap (false, nil)
// yields an empty response id — the machine works in the shared per-account bucket.
func TestResolveRegisteredHost_CapRefusalReturnsEmpty(t *testing.T) {
	vol := types.NewID()
	stub := &stubHostRepo{mintFn: func(_ *volunteer.Host) (bool, error) { return false, nil }}
	svc := newHostIdentityService(stub, newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{}), HostCapPolicy{PerAccount: 1, ActiveWindow: time.Hour})

	if got := svc.resolveRegisteredHost(context.Background(), vol, hostRegisterReq(""), volunteer.HardwareCapabilities{}, time.Now()); got != "" {
		t.Errorf("cap-refused mint = %q, want empty", got)
	}
}

// TestResolveRegisteredHost_MintErrorReturnsEmpty: a mint error (not a cap refusal) also folds
// to an empty response id rather than failing registration.
func TestResolveRegisteredHost_MintErrorReturnsEmpty(t *testing.T) {
	vol := types.NewID()
	stub := &stubHostRepo{mintFn: func(_ *volunteer.Host) (bool, error) {
		return false, apierror.Internal("boom", nil)
	}}
	svc := newHostIdentityService(stub, nil, HostCapPolicy{PerAccount: 10, ActiveWindow: hostActiveWindowUnit})

	if got := svc.resolveRegisteredHost(context.Background(), vol, hostRegisterReq(""), volunteer.HardwareCapabilities{}, time.Now()); got != "" {
		t.Errorf("mint error = %q, want empty (registration still succeeds)", got)
	}
}

// TestResolveRegisteredHost_NilHostRepoReturnsEmpty: with no host repo (the gRPC-plumbing
// unit-test configuration), resolution returns empty and touches nothing.
func TestResolveRegisteredHost_NilHostRepoReturnsEmpty(t *testing.T) {
	svc := newHostIdentityService(nil, nil, HostCapPolicy{})
	if got := svc.resolveRegisteredHost(context.Background(), types.NewID(), hostRegisterReq(""), volunteer.HardwareCapabilities{}, time.Now()); got != "" {
		t.Errorf("nil hostRepo = %q, want empty", got)
	}
}

// --- (d) RequestWorkUnit host validation --------------------------------------

// hostActiveWindowUnit is a plain staleness window for the unit tests (value is irrelevant to
// the in-memory paths; only that it is threaded verbatim into Mint matters).
const hostActiveWindowUnit = 30 * 24 * time.Hour

// hostWorkService builds a *volunteerService whose dispatch cache has a warmed identity for
// the returned volunteer, so RequestWorkUnit resolves auth in memory and the host-validation
// block is the thing under test. The ready pool is empty, so a request that passes validation
// returns an empty (no-work) response with no DB touch.
func hostWorkService(t *testing.T) (*volunteerService, *dispatchCache, ed25519.PublicKey, types.ID) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	volID := types.NewID()
	vol := &volunteer.Volunteer{ID: volID, PublicKey: pub, AvailableRuntimes: []string{leaf.RuntimeNative}}

	cache := newTestCache(&fakeWURepo{}, &fakeLeafRepo{}, &fakeAssignRepo{})
	cache.deps.volunteerRepo = &fakeVolunteerRepo{vols: map[types.ID]*volunteer.Volunteer{volID: vol}}
	cache.putIdentity(vol)

	svc := &volunteerService{
		dispatchCache:      cache,
		logger:             testLogger(),
		now:                time.Now,
		loadEstimator:      newLoadEstimator(defaultLoadEstimatorConfig(), nil),
		maxBatchPerRequest: 5,
	}
	return svc, cache, pub, volID
}

func hostWorkReq(pub ed25519.PublicKey, volID types.ID, hostID string) *lettucev1.RequestWorkUnitRequest {
	pk := make([]byte, 32)
	copy(pk, pub)
	return &lettucev1.RequestWorkUnitRequest{
		VolunteerId:    volID.String(),
		PublicKey:      pk,
		HostId:         hostID,
		MaxAssignments: 1,
	}
}

// TestRequestWorkUnit_UnknownHost_Refused: a non-empty id the head never issued (cold miss →
// definitive not-found) is refused with FailedPrecondition + the pinned prefix.
func TestRequestWorkUnit_UnknownHost_Refused(t *testing.T) {
	svc, cache, pub, volID := hostWorkService(t)
	cache.deps.hostRepo = &fakeHostRepo{hosts: map[types.ID]*volunteer.Host{}} // cold miss → not found
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.RequestWorkUnit(ctx, hostWorkReq(pub, volID, types.NewID().String()))
	if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("unknown host code = %s, want FailedPrecondition: %v", codeOf(err), err)
	}
	if st, _ := status.FromError(err); !strings.HasPrefix(st.Message(), HostUnknownMessagePrefix) {
		t.Errorf("refusal message %q must start with the pinned prefix %q", st.Message(), HostUnknownMessagePrefix)
	}
}

// TestRequestWorkUnit_UnparseableHost_Refused: an unparseable id is treated as definitively
// unknown and refused with the same pinned message.
func TestRequestWorkUnit_UnparseableHost_Refused(t *testing.T) {
	svc, _, pub, volID := hostWorkService(t)
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.RequestWorkUnit(ctx, hostWorkReq(pub, volID, "not-a-uuid"))
	if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("unparseable host code = %s, want FailedPrecondition: %v", codeOf(err), err)
	}
	if st, _ := status.FromError(err); !strings.HasPrefix(st.Message(), HostUnknownMessagePrefix) {
		t.Errorf("refusal message %q must start with the pinned prefix", st.Message())
	}
}

// TestRequestWorkUnit_ForeignHost_Refused: an id owned by a DIFFERENT account (owner mismatch,
// definitive) is refused, never metered under the requesting account.
func TestRequestWorkUnit_ForeignHost_Refused(t *testing.T) {
	svc, cache, pub, volID := hostWorkService(t)
	host := types.NewID()
	cache.putHostOwner(host, types.NewID()) // owned by another account
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	_, err := svc.RequestWorkUnit(ctx, hostWorkReq(pub, volID, host.String()))
	if codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("foreign host code = %s, want FailedPrecondition: %v", codeOf(err), err)
	}
}

// TestRequestWorkUnit_ValidHost_MetersAndBumps: an id owned by THIS account validates (no
// error) and triggers the throttled last-seen bump exactly once.
func TestRequestWorkUnit_ValidHost_MetersAndBumps(t *testing.T) {
	svc, cache, pub, volID := hostWorkService(t)
	host := types.NewID()
	cache.putHostOwner(host, volID) // owned by THIS account
	stub := &stubHostRepo{}
	svc.hostRepo = stub
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	resp, err := svc.RequestWorkUnit(ctx, hostWorkReq(pub, volID, host.String()))
	if err != nil {
		t.Fatalf("a valid owned host should not error: %v", err)
	}
	if resp == nil {
		t.Fatal("a valid host request should return a response")
	}
	if stub.updateLastSeenCalls != 1 {
		t.Errorf("a valid host should bump last-seen exactly once, got %d", stub.updateLastSeenCalls)
	}
}

// TestRequestWorkUnit_EmptyHost_AccountFallback: an empty host id is always valid (the
// per-account fallback bucket) and drives no per-host last-seen bump.
func TestRequestWorkUnit_EmptyHost_AccountFallback(t *testing.T) {
	svc, _, pub, volID := hostWorkService(t)
	stub := &stubHostRepo{}
	svc.hostRepo = stub
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	resp, err := svc.RequestWorkUnit(ctx, hostWorkReq(pub, volID, ""))
	if err != nil {
		t.Fatalf("empty host id (account fallback) should not error: %v", err)
	}
	if resp == nil {
		t.Fatal("an account-fallback request should return a response")
	}
	if stub.updateLastSeenCalls != 0 {
		t.Errorf("no host id means no per-host last-seen bump, got %d", stub.updateLastSeenCalls)
	}
}

// TestRequestWorkUnit_UndeterminableHost_Folds: when ownership cannot be determined (no repo /
// shed), the request folds to the account bucket for THIS request and is NOT refused — a
// post-deploy cold cache must never trigger a fleet-wide discard-and-re-mint storm.
func TestRequestWorkUnit_UndeterminableHost_Folds(t *testing.T) {
	svc, cache, pub, volID := hostWorkService(t)
	cache.deps.hostRepo = nil // ownership undeterminable (cold cache, no oracle read possible)
	svc.hostRepo = nil
	ctx := contextWithGRPCAuthPublicKey(context.Background(), pub)

	resp, err := svc.RequestWorkUnit(ctx, hostWorkReq(pub, volID, types.NewID().String()))
	if err != nil {
		t.Fatalf("an undeterminable host must fold to the account bucket, not refuse: %v", err)
	}
	if resp == nil {
		t.Fatal("a folded request should return a response")
	}
}
