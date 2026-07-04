package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/admission"
	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// testDID is a well-formed did:plc value used throughout the bind/recheck tests.
const testDID = "did:plc:testvolunteer000000000000"

// testCollection is the key-authorization collection the fake head is configured for.
const testCollection = "tech.scios.lettuce.keyAuthorization"

// fakeATProto is a single TLS test server that answers every ATProto endpoint the head
// touches: DID resolution, getRecord, describeRepo, and the PLC audit log. It serves the
// resolver and the PDS from the same origin, so the DID document points its PDS endpoint
// back at this server (whose self-signed cert the paired atproto.Client trusts).
//
// The default DID document resolves the PDS to this server and getRecord returns
// recordValue wrapped in a getRecord envelope. Each response is overridable per test by
// setting the exported fields (guarded by mu for the rare concurrent reader).
type fakeATProto struct {
	server *httptest.Server
	client *atproto.Client

	mu sync.Mutex

	// DID resolution.
	didDocStatus int
	didDoc       string // full DID document JSON; empty => default doc pointing PDS here

	// getRecord.
	recordStatus int
	recordValue  string // the record "value" object JSON; wrapped in an envelope on 200
	recordCID    string
	recordBody   string // full getRecord response body; overrides recordValue/recordCID when set

	// describeRepo (RepoAlive).
	describeStatus int
	describeBody   string

	// PLC audit log.
	auditStatus int
	auditBody   string
}

func newFakeATProto(t *testing.T) *fakeATProto {
	t.Helper()
	f := &fakeATProto{
		didDocStatus:   http.StatusOK,
		recordStatus:   http.StatusOK,
		recordCID:      "bafyreidefaultcid00000000000000000000000000000000000000",
		describeStatus: http.StatusOK,
		describeBody:   "{}",
		auditStatus:    http.StatusOK,
		auditBody:      "[]",
	}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.route))
	t.Cleanup(f.server.Close)
	// The paired client trusts this server's cert (server.Client()) and, because it is a
	// caller-supplied client, bypasses the atproto SSRF dial guard so it can reach the
	// loopback test server — the documented, intended bypass for tests.
	f.client = atproto.NewClient(f.server.URL, f.server.Client(), nil)
	return f
}

func (f *fakeATProto) route(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/log/audit"):
		w.WriteHeader(f.auditStatus)
		_, _ = w.Write([]byte(f.auditBody))
	case strings.Contains(path, "/xrpc/com.atproto.repo.getRecord"):
		f.writeRecord(w)
	case strings.Contains(path, "/xrpc/com.atproto.repo.describeRepo"):
		w.WriteHeader(f.describeStatus)
		_, _ = w.Write([]byte(f.describeBody))
	default:
		// DID document resolution: /{did}.
		f.writeDIDDoc(w)
	}
}

func (f *fakeATProto) writeDIDDoc(w http.ResponseWriter) {
	if f.didDocStatus != http.StatusOK {
		w.WriteHeader(f.didDocStatus)
		return
	}
	doc := f.didDoc
	if doc == "" {
		doc = fmt.Sprintf(`{
			"id": %q,
			"service": [{
				"id": "#atproto_pds",
				"type": "AtprotoPersonalDataServer",
				"serviceEndpoint": %q
			}]
		}`, testDID, f.server.URL)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(doc))
}

func (f *fakeATProto) writeRecord(w http.ResponseWriter) {
	if f.recordStatus != http.StatusOK {
		w.WriteHeader(f.recordStatus)
		if f.recordBody != "" {
			_, _ = w.Write([]byte(f.recordBody))
		}
		return
	}
	if f.recordBody != "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(f.recordBody))
		return
	}
	env := fmt.Sprintf(`{"uri":"at://%s/%s/self","cid":%q,"value":%s}`,
		testDID, testCollection, f.recordCID, f.recordValue)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(env))
}

// setRecordNotFound makes getRecord return the XRPC RecordNotFound error.
func (f *fakeATProto) setRecordNotFound() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordStatus = http.StatusBadRequest
	f.recordBody = `{"error":"RecordNotFound","message":"could not locate record"}`
}

// setDescribeRepoGone makes describeRepo report the account gone.
func (f *fakeATProto) setDescribeRepoGone() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeStatus = http.StatusBadRequest
	f.describeBody = `{"error":"AccountDeactivated","message":"account is deactivated"}`
}

// setDescribeRepoOutage makes describeRepo return a 5xx (liveness unknown).
func (f *fakeATProto) setDescribeRepoOutage() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeStatus = http.StatusInternalServerError
	f.describeBody = `{"error":"InternalServerError"}`
}

// signedRecordValue builds the JSON for a key-authorization record VALUE that verifies
// against pub, signed by priv. label and expiresAt are optional (empty to omit).
func signedRecordValue(t *testing.T, did string, pub ed25519.PublicKey, priv ed25519.PrivateKey, label, expiresAt string) string {
	t.Helper()
	opKey, err := atproto.EncodeEd25519DIDKey(pub)
	if err != nil {
		t.Fatalf("encode did:key: %v", err)
	}
	createdAt := "2026-01-01T00:00:00Z"
	canonical := atproto.CanonicalKeyAuthorizationBytes(did, opKey, label, createdAt)
	sig := ed25519.Sign(priv, canonical)

	rec := atproto.KeyAuthorizationRecord{
		DID:            did,
		OperationalKey: opKey,
		KeySignature:   atproto.Bytes(sig),
		Label:          label,
		CreatedAt:      createdAt,
		ExpiresAt:      expiresAt,
	}
	out, err := json.Marshal(&rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return string(out)
}

// recordingVolunteerRepo is a volunteer.Repository that records the DID-binding writes so
// tests can assert exactly one authoritative outcome per re-check. It is separate from the
// simpler handler_test.go mock so those tests stay untouched.
type recordingVolunteerRepo struct {
	byKey map[string]*volunteer.Volunteer // base64url pubkey -> row
	byID  map[types.ID]*volunteer.Volunteer

	setBinding   []setBindingCall
	checked      []markCheckedCall
	failed       []markFailedCall
	revoked      []types.ID
	frozenUntil  map[types.ID]time.Time
	recheckBatch []*volunteer.Volunteer // returned by ListDIDBindingsForRecheck
}

type setBindingCall struct {
	id      types.ID
	did     string
	uri     string
	cid     string
	boundAt time.Time
}
type markCheckedCall struct {
	id  types.ID
	cid string
}
type markFailedCall struct {
	id         types.ID
	staleAfter int
}

func newRecordingRepo() *recordingVolunteerRepo {
	return &recordingVolunteerRepo{
		byKey:       make(map[string]*volunteer.Volunteer),
		byID:        make(map[types.ID]*volunteer.Volunteer),
		frozenUntil: make(map[types.ID]time.Time),
	}
}

func (r *recordingVolunteerRepo) add(v *volunteer.Volunteer) *volunteer.Volunteer {
	r.byKey[keyString(v.PublicKey)] = v
	r.byID[v.ID] = v
	return v
}

func keyString(pub []byte) string {
	return string(pub)
}

func (r *recordingVolunteerRepo) GetByPublicKey(_ context.Context, publicKey []byte) (*volunteer.Volunteer, error) {
	v, ok := r.byKey[keyString(publicKey)]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (r *recordingVolunteerRepo) SetDIDBinding(_ context.Context, id types.ID, did, uri, cid string, boundAt time.Time) error {
	r.setBinding = append(r.setBinding, setBindingCall{id: id, did: did, uri: uri, cid: cid, boundAt: boundAt})
	return nil
}
func (r *recordingVolunteerRepo) ListDIDBindingsForRecheck(_ context.Context, _ time.Time, _ int) ([]*volunteer.Volunteer, error) {
	return r.recheckBatch, nil
}
func (r *recordingVolunteerRepo) MarkDIDBindingChecked(_ context.Context, id types.ID, cid string, _ time.Time) error {
	r.checked = append(r.checked, markCheckedCall{id: id, cid: cid})
	return nil
}
func (r *recordingVolunteerRepo) MarkDIDBindingCheckFailed(_ context.Context, id types.ID, _ time.Time, staleAfter int) error {
	r.failed = append(r.failed, markFailedCall{id: id, staleAfter: staleAfter})
	return nil
}
func (r *recordingVolunteerRepo) RevokeDIDBinding(_ context.Context, id types.ID, _ time.Time) error {
	r.revoked = append(r.revoked, id)
	return nil
}
func (r *recordingVolunteerRepo) SetDIDFrozenUntil(_ context.Context, id types.ID, until time.Time) error {
	r.frozenUntil[id] = until
	return nil
}

// Remaining Repository methods are unused by these tests.
func (r *recordingVolunteerRepo) Create(context.Context, *volunteer.Volunteer) error { return nil }
func (r *recordingVolunteerRepo) CreateAdmitted(context.Context, *volunteer.Volunteer, *admission.CreateGate) error {
	return nil
}
func (r *recordingVolunteerRepo) GetByID(_ context.Context, id types.ID) (*volunteer.Volunteer, error) {
	return r.byID[id], nil
}
func (r *recordingVolunteerRepo) GetByUserID(context.Context, types.ID) (*volunteer.Volunteer, error) {
	return nil, nil
}
func (r *recordingVolunteerRepo) Update(context.Context, *volunteer.Volunteer) error { return nil }
func (r *recordingVolunteerRepo) UpdateLastSeen(context.Context, types.ID) error     { return nil }
func (r *recordingVolunteerRepo) SetActive(context.Context, types.ID, bool) error    { return nil }
func (r *recordingVolunteerRepo) IncrementWorkUnitsCompleted(context.Context, types.ID) error {
	return nil
}
func (r *recordingVolunteerRepo) IncrementWorkUnitsRejected(context.Context, types.ID) error {
	return nil
}
func (r *recordingVolunteerRepo) List(context.Context, volunteer.VolunteerListFilters, types.PaginationRequest) ([]*volunteer.Volunteer, types.PaginationResponse, error) {
	return nil, types.PaginationResponse{}, nil
}
func (r *recordingVolunteerRepo) MarkInactiveOlderThan(context.Context, time.Duration) (int, error) {
	return 0, nil
}
