package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/config"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/volunteer"
)

// recheckTest wires a DIDRecheckWorker against a fake ATProto server and a recording repo,
// with one bound volunteer queued for recheck.
type recheckTest struct {
	worker *DIDRecheckWorker
	fake   *fakeATProto
	repo   *recordingVolunteerRepo
	vol    *volunteer.Volunteer
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
}

func newRecheckTest(t *testing.T) *recheckTest {
	t.Helper()
	fake := newFakeATProto(t)
	repo := newRecordingRepo()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	boundAt := types.Now().Add(-2 * time.Hour)
	did := testDID
	uri := fmt.Sprintf("at://%s/%s/self", testDID, testCollection)
	status := volunteer.DIDBindingStatusOK
	vol := &volunteer.Volunteer{
		ID:               types.NewID(),
		PublicKey:        pub,
		DID:              &did,
		DIDBindingURI:    &uri,
		DIDBindingStatus: &status,
		DIDBoundAt:       &boundAt,
	}
	repo.add(vol)
	repo.recheckBatch = []*volunteer.Volunteer{vol}

	cfg := config.HeadConfig{Name: "test", DIDBindingEnabled: true}
	worker := NewDIDRecheckWorker(fake.client, repo, cfg, slog.Default())
	return &recheckTest{worker: worker, fake: fake, repo: repo, vol: vol, pub: pub, priv: priv}
}

func (rt *recheckTest) run() {
	rt.worker.recheckOne(context.Background(), rt.vol)
}

// assertOutcome fails unless exactly one of {checked, revoked, failed} happened.
func (rt *recheckTest) assertOutcome(t *testing.T, checked, revoked, failed int) {
	t.Helper()
	if len(rt.repo.checked) != checked {
		t.Errorf("MarkDIDBindingChecked calls = %d, want %d", len(rt.repo.checked), checked)
	}
	if len(rt.repo.revoked) != revoked {
		t.Errorf("RevokeDIDBinding calls = %d, want %d", len(rt.repo.revoked), revoked)
	}
	if len(rt.repo.failed) != failed {
		t.Errorf("MarkDIDBindingCheckFailed calls = %d, want %d", len(rt.repo.failed), failed)
	}
}

func TestRecheck_VerifyPasses_MarksChecked(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.recordValue = signedRecordValue(t, testDID, rt.pub, rt.priv, "", "")
	rt.fake.recordCID = "bafyreifreshcid000000000000000000000000000000000000000000"

	rt.run()

	rt.assertOutcome(t, 1, 0, 0)
	if rt.repo.checked[0].cid != rt.fake.recordCID {
		t.Errorf("refreshed CID = %q, want %q", rt.repo.checked[0].cid, rt.fake.recordCID)
	}
}

func TestRecheck_VerifyFailsSentinel_Revokes(t *testing.T) {
	rt := newRecheckTest(t)
	// Record now authorizes a different key: the stored key is no longer authorized.
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	rt.fake.recordValue = signedRecordValue(t, testDID, otherPub, otherPriv, "", "")

	rt.run()

	rt.assertOutcome(t, 0, 1, 0)
	if rt.repo.revoked[0] != rt.vol.ID {
		t.Errorf("revoked wrong volunteer: %v", rt.repo.revoked[0])
	}
}

func TestRecheck_ExpiredRecord_Revokes(t *testing.T) {
	rt := newRecheckTest(t)
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	rt.fake.recordValue = signedRecordValue(t, testDID, rt.pub, rt.priv, "", past)

	rt.run()

	rt.assertOutcome(t, 0, 1, 0)
}

func TestRecheck_RecordGone_RepoAlive_Revokes(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.setRecordNotFound()
	// describeRepo defaults to 200 => repo alive => volunteer deleted the record.

	rt.run()

	rt.assertOutcome(t, 0, 1, 0)
}

func TestRecheck_RecordGone_AccountGone_Revokes(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.setRecordNotFound()
	rt.fake.setDescribeRepoGone()

	rt.run()

	rt.assertOutcome(t, 0, 1, 0)
}

func TestRecheck_RecordGone_RepoOutage_MarksFailed(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.setRecordNotFound()
	rt.fake.setDescribeRepoOutage()

	rt.run()

	rt.assertOutcome(t, 0, 0, 1)
	if rt.repo.failed[0].staleAfter != rt.worker.cfg.EffectiveDIDStaleAfterFailures() {
		t.Errorf("staleAfter = %d, want %d", rt.repo.failed[0].staleAfter, rt.worker.cfg.EffectiveDIDStaleAfterFailures())
	}
}

func TestRecheck_DIDNotFound_Revokes(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.didDocStatus = 404

	rt.run()

	rt.assertOutcome(t, 0, 1, 0)
}

func TestRecheck_ResolverOutage_MarksFailed(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.didDocStatus = 500

	rt.run()

	rt.assertOutcome(t, 0, 0, 1)
}

func TestRecheck_RecordFetchOutage_MarksFailed(t *testing.T) {
	rt := newRecheckTest(t)
	// getRecord returns a 5xx (not a RecordNotFound XRPC error).
	rt.fake.recordStatus = 503
	rt.fake.recordBody = `{"error":"UpstreamFailure"}`

	rt.run()

	rt.assertOutcome(t, 0, 0, 1)
}

func TestRecheck_MalformedRecord_MarksFailed(t *testing.T) {
	rt := newRecheckTest(t)
	// A record whose value is not a key-authorization object: ambiguous, not a
	// definitive repudiation, so it must degrade rather than hard-revoke.
	rt.fake.recordValue = `"not-an-object"`

	rt.run()

	rt.assertOutcome(t, 0, 0, 1)
}

func TestRecheck_Rotation_FreezesAfterVerify(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.recordValue = signedRecordValue(t, testDID, rt.pub, rt.priv, "", "")
	// PLC audit log: a create op, then a signing-key rotation AFTER the binding was
	// created (bound 2h ago; rotation 1h ago).
	rotatedAt := time.Now().UTC().Add(-1 * time.Hour)
	rt.fake.auditBody = fmt.Sprintf(`[
		{"createdAt":"2026-01-01T00:00:00Z","operation":{"verificationMethods":{"atproto":"zKEY_A"},"services":{"atproto_pds":{"type":"AtprotoPersonalDataServer","endpoint":%q}}}},
		{"createdAt":%q,"operation":{"verificationMethods":{"atproto":"zKEY_B"},"services":{"atproto_pds":{"type":"AtprotoPersonalDataServer","endpoint":%q}}}}
	]`, rt.fake.server.URL, rotatedAt.Format(time.RFC3339), rt.fake.server.URL)

	rt.run()

	// The successful re-verification still stands.
	rt.assertOutcome(t, 1, 0, 0)
	until, ok := rt.repo.frozenUntil[rt.vol.ID]
	if !ok {
		t.Fatal("a post-binding rotation must record a freeze")
	}
	// Freeze deadline is rotatedAt + freeze window.
	wantMin := rotatedAt.Add(time.Duration(rt.worker.cfg.EffectiveDIDRotationFreezeHours()-1) * time.Hour)
	if !until.After(wantMin) {
		t.Errorf("freeze deadline %v is earlier than expected (> %v)", until, wantMin)
	}
}

func TestRecheck_RotationBeforeBinding_NoFreeze(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.recordValue = signedRecordValue(t, testDID, rt.pub, rt.priv, "", "")
	// The only rotation predates the binding (bound 2h ago; rotation 5h ago), so it is
	// not relevant and must not freeze.
	rotatedAt := time.Now().UTC().Add(-5 * time.Hour)
	rt.fake.auditBody = fmt.Sprintf(`[
		{"createdAt":"2020-01-01T00:00:00Z","operation":{"verificationMethods":{"atproto":"zKEY_A"}}},
		{"createdAt":%q,"operation":{"verificationMethods":{"atproto":"zKEY_B"}}}
	]`, rotatedAt.Format(time.RFC3339))

	rt.run()

	rt.assertOutcome(t, 1, 0, 0)
	if len(rt.repo.frozenUntil) != 0 {
		t.Errorf("a pre-binding rotation must not freeze; got %v", rt.repo.frozenUntil)
	}
}

func TestRecheck_NoRotation_NoFreeze(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.recordValue = signedRecordValue(t, testDID, rt.pub, rt.priv, "", "")
	// Audit log with a single op (no predecessor to differ from) => no rotation.
	rt.fake.auditBody = `[{"createdAt":"2026-01-01T00:00:00Z","operation":{"verificationMethods":{"atproto":"zKEY_A"}}}]`

	rt.run()

	rt.assertOutcome(t, 1, 0, 0)
	if len(rt.repo.frozenUntil) != 0 {
		t.Errorf("no rotation must not freeze; got %v", rt.repo.frozenUntil)
	}
}

// TestRecheck_SweepDrainsBatch exercises the full sweep loop against the list repo.
func TestRecheck_SweepDrainsBatch(t *testing.T) {
	rt := newRecheckTest(t)
	rt.fake.recordValue = signedRecordValue(t, testDID, rt.pub, rt.priv, "", "")

	rt.worker.sweep(context.Background())

	rt.assertOutcome(t, 1, 0, 0)
}
