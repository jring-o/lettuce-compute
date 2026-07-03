package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/atproto"
	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// bindTestSetup wires a BindHandler against a fake ATProto server and a recording repo,
// with a registered volunteer whose device key is (pub, priv).
type bindTestSetup struct {
	handler *BindHandler
	fake    *fakeATProto
	repo    *recordingVolunteerRepo
	vol     *volunteer.Volunteer
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
}

func newBindTest(t *testing.T, enabled bool) *bindTestSetup {
	t.Helper()
	fake := newFakeATProto(t)
	repo := newRecordingRepo()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	vol := repo.add(&volunteer.Volunteer{ID: types.NewID(), PublicKey: pub, IsActive: true})

	cfg := config.HeadConfig{Name: "test", DIDBindingEnabled: enabled}
	handler := NewBindHandler(fake.client, repo, cfg, slog.Default())
	return &bindTestSetup{handler: handler, fake: fake, repo: repo, vol: vol, pub: pub, priv: priv}
}

func recordURI(did string) string {
	return fmt.Sprintf("at://%s/%s/self", did, testCollection)
}

// callBind invokes the handler with an explicit authenticated key (bypassing the router's
// Ed25519 wrapper, which is what supplies the key in production).
func (s *bindTestSetup) callBind(did, uri string, authedKey ed25519.PublicKey) *httptest.ResponseRecorder {
	body, _ := json.Marshal(bindDIDRequest{DID: did, RecordURI: uri})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/identity/bind-did", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handler.Handle(w, req, authedKey)
	return w
}

func TestBindDID_Success(t *testing.T) {
	s := newBindTest(t, true)
	s.fake.recordValue = signedRecordValue(t, testDID, s.pub, s.priv, "my-laptop", "")

	w := s.callBind(testDID, recordURI(testDID), s.pub)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp bindDIDResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DID != testDID {
		t.Errorf("did = %q, want %q", resp.DID, testDID)
	}
	if resp.BindingStatus != volunteer.DIDBindingStatusOK {
		t.Errorf("binding_status = %q, want OK", resp.BindingStatus)
	}
	if resp.BoundAt == "" {
		t.Error("expected non-empty bound_at")
	}

	if len(s.repo.setBinding) != 1 {
		t.Fatalf("expected exactly one SetDIDBinding call, got %d", len(s.repo.setBinding))
	}
	call := s.repo.setBinding[0]
	if call.id != s.vol.ID || call.did != testDID || call.uri != recordURI(testDID) {
		t.Errorf("SetDIDBinding recorded wrong args: %+v", call)
	}
	if call.cid != s.fake.recordCID {
		t.Errorf("bound CID = %q, want %q", call.cid, s.fake.recordCID)
	}
	if len(s.repo.frozenUntil) != 0 {
		t.Errorf("first bind must not freeze; got %v", s.repo.frozenUntil)
	}
}

func TestBindDID_DisabledReturns404(t *testing.T) {
	s := newBindTest(t, false) // binding disabled
	s.fake.recordValue = signedRecordValue(t, testDID, s.pub, s.priv, "", "")

	w := s.callBind(testDID, recordURI(testDID), s.pub)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when disabled, got %d", w.Code)
	}
	if len(s.repo.setBinding) != 0 {
		t.Error("nothing must be persisted when disabled")
	}
}

func TestBindDID_RecordURIDIDMismatch(t *testing.T) {
	s := newBindTest(t, true)
	// record_uri authority is a different DID than body.did.
	w := s.callBind(testDID, recordURI("did:plc:someoneelse00000000000000"), s.pub)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.repo.setBinding) != 0 {
		t.Error("nothing must be persisted on validation failure")
	}
}

func TestBindDID_WrongCollection(t *testing.T) {
	s := newBindTest(t, true)
	uri := fmt.Sprintf("at://%s/%s/self", testDID, "com.example.wrong")
	w := s.callBind(testDID, uri, s.pub)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBindDID_MissingFields(t *testing.T) {
	s := newBindTest(t, true)
	w := s.callBind("", "", s.pub)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestBindDID_DIDNotFound(t *testing.T) {
	s := newBindTest(t, true)
	s.fake.didDocStatus = http.StatusNotFound
	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unresolvable DID, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBindDID_ResolverOutageReturns502(t *testing.T) {
	s := newBindTest(t, true)
	s.fake.didDocStatus = http.StatusInternalServerError
	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for resolver outage, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBindDID_RecordNotFound(t *testing.T) {
	s := newBindTest(t, true)
	s.fake.setRecordNotFound()
	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing record, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBindDID_KeyMismatch(t *testing.T) {
	s := newBindTest(t, true)
	// Record authorizes a DIFFERENT operational key than the authenticated one.
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	s.fake.recordValue = signedRecordValue(t, testDID, otherPub, otherPriv, "", "")

	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for key mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.repo.setBinding) != 0 {
		t.Error("nothing must be persisted on verification failure")
	}
}

func TestBindDID_BadSignature(t *testing.T) {
	s := newBindTest(t, true)
	// Correct operational key, but the signature is over the wrong bytes.
	opKey, err := atproto.EncodeEd25519DIDKey(s.pub)
	if err != nil {
		t.Fatalf("encode did:key: %v", err)
	}
	rec := map[string]any{
		"did":            testDID,
		"operationalKey": opKey,
		"keySignature":   map[string]string{"$bytes": "AAAA"},
		"createdAt":      "2026-01-01T00:00:00Z",
	}
	raw, _ := json.Marshal(rec)
	s.fake.recordValue = string(raw)

	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for bad signature, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBindDID_Expired(t *testing.T) {
	s := newBindTest(t, true)
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	s.fake.recordValue = signedRecordValue(t, testDID, s.pub, s.priv, "", past)

	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for expired record, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBindDID_UnregisteredVolunteer(t *testing.T) {
	s := newBindTest(t, true)
	// A key that verifies against the record but is NOT registered as a volunteer.
	strangerPub, strangerPriv, _ := ed25519.GenerateKey(rand.Reader)
	s.fake.recordValue = signedRecordValue(t, testDID, strangerPub, strangerPriv, "", "")

	w := s.callBind(testDID, recordURI(testDID), strangerPub)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unregistered volunteer, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.repo.setBinding) != 0 {
		t.Error("nothing must be persisted for an unregistered volunteer")
	}
}

func TestBindDID_IdentityMoveFreezes(t *testing.T) {
	s := newBindTest(t, true)
	// The device key is already bound to a DIFFERENT DID.
	oldDID := "did:plc:oldidentity0000000000000"
	s.vol.DID = &oldDID
	s.fake.recordValue = signedRecordValue(t, testDID, s.pub, s.priv, "", "")

	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for allowed re-bind, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.repo.setBinding) != 1 || s.repo.setBinding[0].did != testDID {
		t.Fatalf("expected the new DID to be bound, got %+v", s.repo.setBinding)
	}
	until, ok := s.repo.frozenUntil[s.vol.ID]
	if !ok {
		t.Fatal("identity move must set a rotation freeze")
	}
	if !until.After(types.Now()) {
		t.Errorf("freeze deadline %v should be in the future", until)
	}
}

func TestBindDID_SameDIDRebindDoesNotFreeze(t *testing.T) {
	s := newBindTest(t, true)
	same := testDID
	s.vol.DID = &same
	s.fake.recordValue = signedRecordValue(t, testDID, s.pub, s.priv, "", "")

	w := s.callBind(testDID, recordURI(testDID), s.pub)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(s.repo.frozenUntil) != 0 {
		t.Errorf("re-binding the same DID must not freeze; got %v", s.repo.frozenUntil)
	}
}
