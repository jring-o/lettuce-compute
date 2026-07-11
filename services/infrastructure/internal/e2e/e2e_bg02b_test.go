//go:build integration

package e2e_test

// e2e_bg02b_test.go is the DB-backed end-to-end suite for slice-5 external-output
// fetch-and-verify (design doc §10; BG-02b). It exercises the SHIPPED production surfaces —
// the gRPC submit gate (internal/server), the content-verification worker
// (internal/contentverify), the single transitioner, and the results/attestation HTTP
// endpoints — against a real Postgres. It maps to design §10.11 letters:
//
//   (b) TestBG02b_b_EndToEndVerifiedFlow          — the honest fetch-and-verify happy path
//   (c) TestBG02b_c_AttackRegression              — the BG-02b attack (held→failed, no credit)
//   (d) TestBG02b_d_ServedMismatchIsDisagreedNotSlash — a served mismatch is an ordinary DISAGREED
//   (e) TestBG02b_e_CoverageSemanticsOverDispatch — a held ref frees its coverage slot
//   (f) TestBG02b_f_FetchTimeRecheckOptOut        — a mid-window opt-out is honored, never fetched
//   (g) TestBG02b_g_LatePromotionUnitFinalized    — a finalized unit's seat is gone
//   (h) TestBG02b_h_DefaultOffEquivalence         — knob defaulted: ref refused, inline intact
//   (j) TestBG02b_j_ResultsAPIFilter              — the results API serves the two new states
//
// The httptest-origin trick (§10.5 test seam): the D10 URL rules refuse IP-literal hosts and
// non-443 ports, so a raw httptest URL can never pass the allowlist. Instead the leaf allowlists
// the DNS name example.com, results submit URLs like https://example.com/... (the httptest TLS
// cert covers example.com), and the worker's INJECTED client always dials the loopback test
// origin regardless of the requested host. Production code is never weakened — the injected
// client is the sanctioned seam (bgInjectedClient).

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/infrastructure/internal/contentverify"
	"github.com/lettuce-compute/infrastructure/internal/leaf"
	"github.com/lettuce-compute/infrastructure/internal/result"
	"github.com/lettuce-compute/infrastructure/internal/server"
	"github.com/lettuce-compute/infrastructure/internal/types"
	"github.com/lettuce-compute/infrastructure/internal/workunit"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- shared BG-02b helpers ---

// bgContentWorker builds a content-verification worker for these tests: the injected
// httptest-TLS client is the §10.5 test seam, fetching is enabled, the byte cap is the 100 MB
// production default, and the EvaluateFunc closes over the harness transitioner so a promoted
// ref re-adjudicates its unit exactly as a submit-time evaluate would (credit + attestation
// writes included).
func bgContentWorker(env *testEnv, client *http.Client) *contentverify.Worker {
	return contentverify.NewWorker(
		env.pool, client, true, 104857600,
		func(ctx context.Context, workUnitID types.ID) error {
			_, err := env.transitioner.Evaluate(ctx, workUnitID)
			return err
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// bgStartWorker runs the worker (immediate sweep + ticker) in a goroutine and returns a stop
// func that cancels it AND waits for the goroutine to exit, so a test's deferred table cleanup
// never races an in-flight sweep. Always defer (or call) the returned func before cleanup.
func bgStartWorker(ctx context.Context, w *contentverify.Worker) func() {
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		w.Start(wctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

// bgWaitFor polls cond until it returns true or the deadline lapses. The immediate sweep is
// fast, so a few seconds is ample; a timeout fails the test.
func bgWaitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, desc)
}

// bgInjectedClient wraps an httptest-TLS origin as the worker's injected fetch client: it
// trusts the origin's test CA and ALWAYS dials the origin's real listener regardless of the
// requested host:port, so a submitted URL on the allowlisted host example.com (port 443)
// satisfies the D10 contract while the fetch still reaches the loopback test server. The
// production guarded client (with the netguard dial screen) is never touched — THIS injected
// client is the sanctioned §10.5 test seam.
func bgInjectedClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	client := srv.Client()
	tr := client.Transport.(*http.Transport)
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	if tr.TLSClientConfig != nil {
		// The httptest cert covers DNS name example.com; force it as the handshake ServerName
		// so TLS verification passes against the allowlisted host.
		tr.TLSClientConfig.ServerName = "example.com"
	}
	return client
}

// bgResultRow is one result row's content-verification columns.
type bgResultRow struct {
	status      string
	verifiedCk  *string
	outputCk    string
	nextAttempt *time.Time
	lastError   *string
}

func bgReadResult(t *testing.T, env *testEnv, ctx context.Context, resultID string) bgResultRow {
	t.Helper()
	var row bgResultRow
	if err := env.pool.QueryRow(ctx, `
		SELECT validation_status, verified_output_checksum, output_checksum,
		       content_fetch_next_attempt_at, content_fetch_last_error
		FROM results WHERE id = $1`, resultID).
		Scan(&row.status, &row.verifiedCk, &row.outputCk, &row.nextAttempt, &row.lastError); err != nil {
		t.Fatalf("read result %s: %v", resultID, err)
	}
	return row
}

func bgUnitState(t *testing.T, env *testEnv, ctx context.Context, wuID string) string {
	t.Helper()
	var s string
	if err := env.pool.QueryRow(ctx, "SELECT state FROM work_units WHERE id = $1", wuID).Scan(&s); err != nil {
		t.Fatalf("read unit %s state: %v", wuID, err)
	}
	return s
}

func bgCountPending(t *testing.T, env *testEnv, ctx context.Context, wuID string) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM results WHERE work_unit_id = $1 AND validation_status = 'PENDING'", wuID).Scan(&n); err != nil {
		t.Fatalf("count pending for unit %s: %v", wuID, err)
	}
	return n
}

func bgCountCredit(t *testing.T, env *testEnv, ctx context.Context, query string, args ...any) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count credit (%s): %v", query, err)
	}
	return n
}

// bgTrustScore returns the subject's quorum-power score, or 0 when it has no volunteer_trust
// row (a fresh, unseeded subject).
func bgTrustScore(t *testing.T, env *testEnv, ctx context.Context, subject string) int {
	t.Helper()
	var score int
	if err := env.pool.QueryRow(ctx,
		"SELECT COALESCE((SELECT score FROM volunteer_trust WHERE subject = $1), 0)", subject).Scan(&score); err != nil {
		t.Fatalf("read trust score for %q: %v", subject, err)
	}
	return score
}

// bgExternalOutputValConfig opts a leaf into external output references on the given allowlist
// (EXACT comparison — refs are EXACT-only). redundancy sets target == quorum == redundancy.
func bgExternalOutputValConfig(redundancy int, hosts ...string) leaf.ValidationConfig {
	return leaf.ValidationConfig{
		RedundancyFactor:    redundancy,
		AgreementThreshold:  1.0,
		ComparisonMode:      "EXACT",
		MaxRetries:          3,
		AllowExternalOutput: true,
		ExternalOutputHosts: hosts,
	}
}

// bgGenerateOne generates a single param-sweep work unit on the leaf.
func bgGenerateOne(t *testing.T, leafURL string) {
	t.Helper()
	resp := httpReq(t, "POST", leafURL+"/work-units/generate", workunit.GenerateRequest{
		ParameterSpace: map[string]interface{}{"x": []interface{}{float64(1)}},
	})
	requireStatus(t, resp, http.StatusAccepted, "generate work unit")
	resp.Body.Close()
}

// bgResultsFilterHas reports whether the results-list endpoint, filtered by wantStatus, returns
// the given result id (and asserts its reported status matches).
func bgResultsFilterHas(t *testing.T, leafURL, wantStatus, resultID string) bool {
	t.Helper()
	resp := httpReq(t, "GET", leafURL+"/results?validation_status="+wantStatus, nil)
	requireStatus(t, resp, http.StatusOK, "list results by status "+wantStatus)
	var body struct {
		Data []struct {
			ID               string `json:"id"`
			ValidationStatus string `json:"validation_status"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &body)
	for _, r := range body.Data {
		if r.ID == resultID {
			if r.ValidationStatus != wantStatus {
				t.Fatalf("result %s status = %q, want %q", resultID, r.ValidationStatus, wantStatus)
			}
			return true
		}
	}
	return false
}

// bgExecMeta is the trivial execution metadata every BG-02b submit carries.
func bgExecMeta() *lettucev1.ExecutionMetadata {
	return &lettucev1.ExecutionMetadata{WallClockSeconds: 1, CpuSecondsUser: 1, CpuCoresUsed: 1}
}

// --- (b) end-to-end verified flow ---

// TestBG02b_b_EndToEndVerifiedFlow is §10.11(b). Knob on, an opted-in EXACT leaf (allowlist
// example.com, redundancy 1), an allowlisted httptest-TLS origin serving the exact bytes B. A
// submitted reference is HELD and contributes +0 to the quorum (the unit does not complete),
// then the worker fetches + hashes the bytes, promotes the row PENDING on the HEAD-computed
// checksum (overwriting the claim), and the post-promotion Evaluate validates + credits the
// unit; the v2 attestation carries the verified checksum and the public verify endpoint accepts
// it.
func TestBG02b_b_EndToEndVerifiedFlow(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	server.SetContentFetchPolicy(env.volunteerSvc, true) // knob ON

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The exact bytes the origin serves; the volunteer claims THIS checksum honestly.
	bytesB := []byte(`{"answer":"verified-external-output"}`)
	checksumB := sha256Hex(bytesB)

	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytesB)
	}))
	defer srv.Close()
	client := bgInjectedClient(t, srv)

	userID := createTestUser(t, env.pool, ctx, "bg02b-b")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b Verified Flow", Description: "end-to-end fetch-and-verify",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(1, "example.com"),
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()
	bgGenerateOne(t, leafURL)

	pubKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, pubKey, "BG02b Vol")
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, pubKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volID, PublicKey: pubKey})
	if err != nil {
		t.Fatalf("request work unit: %v", err)
	}
	wuID := wuResp.Assignments[0].WorkUnitId
	ensureRunStart(t, env.pool, env.grpc, ctx, volID, pubKey, wuID)

	submitResp, err := env.grpc.SubmitResult(signFor(t, ctx, pubKey), &lettucev1.SubmitResultRequest{
		WorkUnitId:           wuID,
		VolunteerId:          volID,
		PublicKey:            pubKey,
		OutputDataUrl:        "https://example.com/results/wu-" + wuID + ".json",
		OutputChecksumSha256: checksumB,
		Metadata:             bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("submit reference: %v", err)
	}
	if !submitResp.Accepted {
		t.Fatalf("reference not accepted: %s", submitResp.Message)
	}
	resultID := submitResp.ResultId

	// Assert IN ORDER, part 1 (pre-worker): held + fetch scheduled, unit not completed, +0.
	row := bgReadResult(t, env, ctx, resultID)
	if row.status != string(result.ValidationAwaitingContentVerification) {
		t.Fatalf("held row status = %q, want AWAITING_CONTENT_VERIFICATION", row.status)
	}
	if row.nextAttempt == nil {
		t.Fatal("content_fetch_next_attempt_at not set on the held row")
	}
	if st := bgUnitState(t, env, ctx, wuID); st != "QUEUED" {
		t.Fatalf("unit state = %q before verification, want QUEUED (a held ref must not complete it)", st)
	}
	// §10.11(vii): a held insert contributes +0 to the quorum — zero PENDING results end to end.
	if n := bgCountPending(t, env, ctx, wuID); n != 0 {
		t.Fatalf("PENDING result count = %d before verification, want 0 (the §10.11 vii +0 arithmetic)", n)
	}

	// Part 2: one worker tick promotes the ref and the post-promotion Evaluate validates.
	stop := bgStartWorker(ctx, bgContentWorker(env, client))
	defer stop()
	bgWaitFor(t, 5*time.Second, "unit validated after content verification", func() bool {
		return bgUnitState(t, env, ctx, wuID) == "VALIDATED"
	})

	// The row was promoted PENDING on the HEAD-computed hash, then validated to AGREED; the
	// verified + output checksums both carry that hash and the fetch bookkeeping is cleared.
	row = bgReadResult(t, env, ctx, resultID)
	if row.verifiedCk == nil || *row.verifiedCk != checksumB {
		t.Fatalf("verified_output_checksum = %v, want %s (the ONLY key a ref votes on)", row.verifiedCk, checksumB)
	}
	if row.outputCk != checksumB {
		t.Fatalf("output_checksum = %q after promotion, want %s (head hash overwrites the claim)", row.outputCk, checksumB)
	}
	if row.nextAttempt != nil {
		t.Fatalf("content_fetch_next_attempt_at not cleared after promotion: %v", *row.nextAttempt)
	}
	if row.lastError != nil {
		t.Fatalf("content_fetch_last_error not cleared after promotion: %v", *row.lastError)
	}
	if row.status != string(result.ValidationAgreed) {
		t.Fatalf("result status = %q, want AGREED (promoted PENDING then validated)", row.status)
	}
	if hits == 0 {
		t.Fatal("origin was never fetched")
	}

	// Credit granted (a credit_ledger row for this unit + volunteer).
	if n := bgCountCredit(t, env, ctx,
		"SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1 AND volunteer_id = $2", wuID, volID); n != 1 {
		t.Fatalf("credit_ledger rows = %d, want 1", n)
	}

	// The v2 attestation for this result carries the verified checksum, and the public verify
	// endpoint accepts its signature.
	var attID, attOutputCk string
	var schemaVersion int
	if err := env.pool.QueryRow(ctx,
		"SELECT id, output_checksum, schema_version FROM credit_attestations WHERE result_id = $1 AND validation_outcome = 'AGREED'",
		resultID).Scan(&attID, &attOutputCk, &schemaVersion); err != nil {
		t.Fatalf("query attestation: %v", err)
	}
	if schemaVersion != 2 {
		t.Fatalf("attestation schema_version = %d, want 2", schemaVersion)
	}
	if attOutputCk != checksumB {
		t.Fatalf("attestation output_checksum = %q, want %s", attOutputCk, checksumB)
	}
	verResp := httpReq(t, "POST", env.httpURL+"/api/v1/attestations/verify", map[string]string{"attestation_id": attID})
	requireStatus(t, verResp, http.StatusOK, "verify attestation")
	var ver struct {
		SignatureValid bool `json:"signature_valid"`
	}
	decodeJSON(t, verResp, &ver)
	if !ver.SignatureValid {
		t.Fatal("attestation signature_valid = false, want true")
	}
}

// --- (c) the BG-02b attack regression (headline) ---

// TestBG02b_c_AttackRegression is §10.11(c). Knob ON, an opted-in EXACT leaf, target/quorum 2:
// two colluding volunteers submit references sharing ONE fabricated claimed checksum for bytes
// never served; the origin 404s both URLs. On PRE-FIX code both refs landed PENDING, grouped on
// the identical claimed string under EXACT comparison, VALIDATED the unit and were credited (the
// e2e_f20 collusion shape inverted). Post-fix: both are held then terminal
// CONTENT_VERIFICATION_FAILED, the unit never validates, no credit is granted. The knob-OFF half
// then shows the front door: with the knob off the reference is refused outright at submit.
func TestBG02b_c_AttackRegression(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Knob-ON half ---
	server.SetContentFetchPolicy(env.volunteerSvc, true)

	fabricatedChecksum := strings.Repeat("ab", 32) // 64 hex chars, no bytes behind it

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 404 both URLs — nothing is ever served
	}))
	defer srv.Close()
	client := bgInjectedClient(t, srv)

	userID := createTestUser(t, env.pool, ctx, "bg02b-c")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b Attack", Description: "colluding refs on a fabricated checksum",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(2, "example.com"), // target/quorum 2
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()
	bgGenerateOne(t, leafURL)

	// Two colluding volunteers on the SAME unit (A reserves; B is a redundant copy).
	volAKey := []byte(genVolunteerKey(t))
	volBKey := []byte(genVolunteerKey(t))
	volAID := registerVolunteer(t, env, ctx, volAKey, "BG02b Attacker A")
	volBID := registerVolunteer(t, env, ctx, volBKey, "BG02b Attacker B")
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volAID, PublicKey: volAKey})
	if err != nil {
		t.Fatalf("vol A request: %v", err)
	}
	wuID := wuResp.Assignments[0].WorkUnitId
	runStartWU(t, env, ctx, wuID, volAID, volAKey)
	createRedundantAssignment(t, env.pool, ctx, wuID, types.MustParseID(volBID))

	subA, err := env.grpc.SubmitResult(signFor(t, ctx, volAKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volAID, PublicKey: volAKey,
		OutputDataUrl: "https://example.com/results/attack-a.json", OutputChecksumSha256: fabricatedChecksum,
		Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("vol A submit: %v", err)
	}
	subB, err := env.grpc.SubmitResult(signFor(t, ctx, volBKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volBID, PublicKey: volBKey,
		OutputDataUrl: "https://example.com/results/attack-b.json", OutputChecksumSha256: fabricatedChecksum,
		Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("vol B submit: %v", err)
	}
	if !subA.Accepted || !subB.Accepted {
		t.Fatalf("references not accepted: A=%q B=%q", subA.Message, subB.Message)
	}

	// Both held before the tick (no PENDING vote on the claimed string).
	if got := bgReadResult(t, env, ctx, subA.ResultId).status; got != string(result.ValidationAwaitingContentVerification) {
		t.Fatalf("A held status = %q, want AWAITING_CONTENT_VERIFICATION", got)
	}
	if got := bgReadResult(t, env, ctx, subB.ResultId).status; got != string(result.ValidationAwaitingContentVerification) {
		t.Fatalf("B held status = %q, want AWAITING_CONTENT_VERIFICATION", got)
	}

	// Worker tick → both fetches 404 → permanent HTTP_STATUS terminal.
	stop := bgStartWorker(ctx, bgContentWorker(env, client))
	bgWaitFor(t, 6*time.Second, "both references terminally failed", func() bool {
		return bgReadResult(t, env, ctx, subA.ResultId).status == string(result.ValidationContentVerificationFailed) &&
			bgReadResult(t, env, ctx, subB.ResultId).status == string(result.ValidationContentVerificationFailed)
	})
	stop()

	for _, rid := range []string{subA.ResultId, subB.ResultId} {
		row := bgReadResult(t, env, ctx, rid)
		if row.lastError == nil || !strings.HasPrefix(*row.lastError, contentverify.CodeHTTPStatus) {
			t.Fatalf("result %s last_error = %v, want prefix %s", rid, row.lastError, contentverify.CodeHTTPStatus)
		}
	}
	if st := bgUnitState(t, env, ctx, wuID); st == "VALIDATED" {
		t.Fatal("unit VALIDATED on a fabricated claimed checksum — the BG-02b hole is open")
	}
	if n := bgCountCredit(t, env, ctx, "SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1", wuID); n != 0 {
		t.Fatalf("credit_ledger rows for the attacked unit = %d, want 0", n)
	}

	// --- Knob-OFF half (the front door): a reference is refused OUTRIGHT at submit. ---
	server.SetContentFetchPolicy(env.volunteerSvc, false)
	proj2 := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b Attack KnobOff", Description: "front-door refusal",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(1, "example.com"),
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leaf2URL := env.httpURL + "/api/v1/leafs/" + proj2.ID.String()
	bgGenerateOne(t, leaf2URL)
	koVolKey := []byte(genVolunteerKey(t))
	koVolID := registerVolunteer(t, env, ctx, koVolKey, "BG02b KnobOff Vol")
	wu2, err := env.grpc.RequestWorkUnit(signFor(t, ctx, koVolKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: koVolID, PublicKey: koVolKey})
	if err != nil {
		t.Fatalf("knob-off request: %v", err)
	}
	wu2ID := wu2.Assignments[0].WorkUnitId
	ensureRunStart(t, env.pool, env.grpc, ctx, koVolID, koVolKey, wu2ID)
	_, err = env.grpc.SubmitResult(signFor(t, ctx, koVolKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wu2ID, VolunteerId: koVolID, PublicKey: koVolKey,
		OutputDataUrl: "https://example.com/results/knob-off.json", OutputChecksumSha256: fabricatedChecksum,
		Metadata: bgExecMeta(),
	})
	if err == nil {
		t.Fatal("knob-off reference submit was accepted; want a FailedPrecondition refusal at the front door")
	}
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Fatalf("knob-off submit error code = %s, want FailedPrecondition", st.Code())
	}
}

// --- (d) served mismatch is a DISAGREED, not a slash (the F2 regression) ---

// TestBG02b_d_ServedMismatchIsDisagreedNotSlash is §10.11(d). An opted-in EXACT leaf, target 3
// quorum 2. Volunteers A+B submit identical INLINE bytes; volunteer C submits a reference
// claiming A/B's checksum, but C's origin serves DIFFERENT bytes. The worker promotes C on its
// SERVED hash (never the claim), then the honest A+B quorum validates the unit with C in the
// minority: A+B AGREED + credited, C DISAGREED + no credit. Crucially NO slash artifact exists
// — the slice-5 fetch worker never calls trust.Slash (a served-vs-claimed divergence is not
// provable fraud; §10.7). The interleave (A inline, C held+promoted, THEN B inline) is what puts
// all three results PENDING at one Evaluate, which any claimed-vs-served gate would instead FAIL
// or sanction.
func TestBG02b_d_ServedMismatchIsDisagreedNotSlash(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	server.SetContentFetchPolicy(env.volunteerSvc, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	honestBytes := []byte(`{"v":1}`)
	honestChecksum := sha256Hex(honestBytes)
	servedBytes := []byte(`{"v":2}`) // what C's origin actually serves — different content
	servedChecksum := sha256Hex(servedBytes)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(servedBytes)
	}))
	defer srv.Close()
	client := bgInjectedClient(t, srv)

	userID := createTestUser(t, env.pool, ctx, "bg02b-d")
	// target 3, quorum 2, threshold 0.6 so the 2-of-3 A+B majority (ratio 0.667) validates with C
	// dissenting. The threshold must exceed 0.5 (the validator refuses a redundant leaf that
	// could validate on a minority) yet stay at or below 2/3 so a 2-of-3 majority still passes.
	valCfg := leaf.ValidationConfig{
		RedundancyFactor:    3,
		TargetCopies:        3,
		MinQuorum:           2,
		AgreementThreshold:  0.6,
		ComparisonMode:      "EXACT",
		MaxRetries:          3,
		AllowExternalOutput: true,
		ExternalOutputHosts: []string{"example.com"},
	}
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b Served Mismatch", Description: "served mismatch is a DISAGREED not a slash",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		valCfg, leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()
	bgGenerateOne(t, leafURL)

	volAKey := []byte(genVolunteerKey(t))
	volBKey := []byte(genVolunteerKey(t))
	volCKey := []byte(genVolunteerKey(t))
	volAID := registerVolunteer(t, env, ctx, volAKey, "BG02b Honest A")
	volBID := registerVolunteer(t, env, ctx, volBKey, "BG02b Honest B")
	volCID := registerVolunteer(t, env, ctx, volCKey, "BG02b Ref C")

	// A reserves the unit; B and C become live redundant copies (bare history rows). A live
	// copy (B) held open across the C-promotion Evaluate is what keeps the unit WAITING rather
	// than rejecting the A/C 1-1 split.
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volAID, PublicKey: volAKey})
	if err != nil {
		t.Fatalf("A request: %v", err)
	}
	wuID := wuResp.Assignments[0].WorkUnitId
	runStartWU(t, env, ctx, wuID, volAID, volAKey)
	createRedundantAssignment(t, env.pool, ctx, wuID, types.MustParseID(volBID))
	createRedundantAssignment(t, env.pool, ctx, wuID, types.MustParseID(volCID))

	// A submits identical honest INLINE bytes.
	subA, err := env.grpc.SubmitResult(signFor(t, ctx, volAKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volAID, PublicKey: volAKey,
		OutputData: honestBytes, OutputChecksumSha256: honestChecksum, Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("A submit: %v", err)
	}
	// C submits a REFERENCE claiming A/B's honest checksum; the origin serves different bytes.
	subC, err := env.grpc.SubmitResult(signFor(t, ctx, volCKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volCID, PublicKey: volCKey,
		OutputDataUrl: "https://example.com/results/mismatch-c.json", OutputChecksumSha256: honestChecksum,
		Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("C submit: %v", err)
	}
	if !subC.Accepted {
		t.Fatalf("C reference not accepted: %s", subC.Message)
	}

	// Capture C's trust subject + score BEFORE the tick (stamped at submit); it must not move.
	var cSubject string
	if err := env.pool.QueryRow(ctx, "SELECT trust_subject FROM results WHERE id = $1", subC.ResultId).Scan(&cSubject); err != nil {
		t.Fatalf("read C trust_subject: %v", err)
	}
	scoreBefore := bgTrustScore(t, env, ctx, cSubject)

	// Worker tick → C promoted PENDING on the SERVED hash, despite the claim divergence.
	stop := bgStartWorker(ctx, bgContentWorker(env, client))
	bgWaitFor(t, 5*time.Second, "C promoted on its served hash", func() bool {
		row := bgReadResult(t, env, ctx, subC.ResultId)
		return row.verifiedCk != nil && *row.verifiedCk == servedChecksum
	})
	stop()

	rowC := bgReadResult(t, env, ctx, subC.ResultId)
	if rowC.verifiedCk == nil || *rowC.verifiedCk != servedChecksum {
		t.Fatalf("C verified_output_checksum = %v, want served %s (promotes on served, never the claim)", rowC.verifiedCk, servedChecksum)
	}

	// B submits the honest inline bytes, completing the A+B quorum; Evaluate now reads all three
	// PENDING and validates on the A+B majority with C in the minority.
	subB, err := env.grpc.SubmitResult(signFor(t, ctx, volBKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volBID, PublicKey: volBKey,
		OutputData: honestBytes, OutputChecksumSha256: honestChecksum, Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("B submit: %v", err)
	}

	if st := bgUnitState(t, env, ctx, wuID); st != "VALIDATED" {
		t.Fatalf("unit state = %q, want VALIDATED on the A+B quorum", st)
	}
	if got := bgReadResult(t, env, ctx, subA.ResultId).status; got != string(result.ValidationAgreed) {
		t.Fatalf("A status = %q, want AGREED", got)
	}
	if got := bgReadResult(t, env, ctx, subB.ResultId).status; got != string(result.ValidationAgreed) {
		t.Fatalf("B status = %q, want AGREED", got)
	}
	if got := bgReadResult(t, env, ctx, subC.ResultId).status; got != string(result.ValidationDisagreed) {
		t.Fatalf("C status = %q, want DISAGREED (a served mismatch is an ordinary disagreement)", got)
	}
	if n := bgCountCredit(t, env, ctx,
		"SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1 AND volunteer_id = $2", wuID, volCID); n != 0 {
		t.Fatalf("C credit_ledger rows = %d, want 0", n)
	}
	if n := bgCountCredit(t, env, ctx,
		"SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1 AND volunteer_id IN ($2, $3)", wuID, volAID, volBID); n != 2 {
		t.Fatalf("A+B credit_ledger rows = %d, want 2", n)
	}

	// No slash artifact: zero credit_adjustments (the slash/clawback table) and C's trust score
	// unchanged. The fetch worker must never sanction a served-vs-claimed divergence.
	if n := bgCountCredit(t, env, ctx, "SELECT COUNT(*) FROM credit_adjustments"); n != 0 {
		t.Fatalf("credit_adjustments rows = %d, want 0 (no slash fires from the fetch worker)", n)
	}
	if scoreAfter := bgTrustScore(t, env, ctx, cSubject); scoreAfter != scoreBefore {
		t.Fatalf("C trust score moved from %d to %d; the fetch worker must never slash", scoreBefore, scoreAfter)
	}
}

// --- (e) coverage semantics: a held ref frees its coverage slot (deliberate over-dispatch) ---

// TestBG02b_e_CoverageSemanticsOverDispatch is §10.11(e). While a reference is HELD (not
// PENDING), it frees its coverage slot (§10.0 item 5), so the unit is dispatchable again: a
// second registered volunteer's RequestWorkUnit receives the SAME unit. The over-dispatch during
// the verification window is deliberate and observable.
func TestBG02b_e_CoverageSemanticsOverDispatch(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	server.SetContentFetchPolicy(env.volunteerSvc, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "bg02b-e")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b Coverage", Description: "a held ref frees its coverage slot",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(1, "example.com"),
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()
	bgGenerateOne(t, leafURL)

	// Volunteer A reserves the sole unit and submits a REFERENCE (held).
	volAKey := []byte(genVolunteerKey(t))
	volAID := registerVolunteer(t, env, ctx, volAKey, "BG02b Coverage A")
	wuA, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volAID, PublicKey: volAKey})
	if err != nil {
		t.Fatalf("A request: %v", err)
	}
	wuID := wuA.Assignments[0].WorkUnitId
	ensureRunStart(t, env.pool, env.grpc, ctx, volAID, volAKey, wuID)
	subA, err := env.grpc.SubmitResult(signFor(t, ctx, volAKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volAID, PublicKey: volAKey,
		OutputDataUrl: "https://example.com/results/coverage-a.json", OutputChecksumSha256: strings.Repeat("cd", 32),
		Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("A submit: %v", err)
	}
	if got := bgReadResult(t, env, ctx, subA.ResultId).status; got != string(result.ValidationAwaitingContentVerification) {
		t.Fatalf("A reference status = %q, want AWAITING_CONTENT_VERIFICATION", got)
	}

	// No worker runs. A second volunteer B must be dispatched the SAME held unit — the held ref
	// freed its coverage slot, so the unit is dispatchable again.
	volBKey := []byte(genVolunteerKey(t))
	volBID := registerVolunteer(t, env, ctx, volBKey, "BG02b Coverage B")
	var wuBID string
	bgWaitFor(t, 5*time.Second, "second volunteer receives the same held unit", func() bool {
		wuB, rerr := env.grpc.RequestWorkUnit(signFor(t, ctx, volBKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volBID, PublicKey: volBKey})
		if rerr != nil || len(wuB.Assignments) == 0 {
			return false
		}
		wuBID = wuB.Assignments[0].WorkUnitId
		return true
	})
	if wuBID != wuID {
		t.Fatalf("second volunteer got unit %s, want the same held unit %s (deliberate over-dispatch)", wuBID, wuID)
	}
}

// --- (f) fetch-time re-check honors a mid-window opt-out ---

// TestBG02b_f_FetchTimeRecheckOptOut is §10.11(f). A reference is submitted while opted in, then
// the owner opts OUT (allow_external_output cleared AND the allowlist removed — the validator
// refuses hosts-without-opt-in). The worker's fetch-time re-check against the CURRENT leaf config
// terminates the row CONTENT_VERIFICATION_FAILED with a LEAF_OPTED_OUT reason and NEVER contacts
// the origin (the check short-circuits before any fetch).
func TestBG02b_f_FetchTimeRecheckOptOut(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	server.SetContentFetchPolicy(env.volunteerSvc, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	served := []byte(`{"served":true}`)
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(served)
	}))
	defer srv.Close()
	client := bgInjectedClient(t, srv)

	userID := createTestUser(t, env.pool, ctx, "bg02b-f")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b OptOut", Description: "fetch-time re-check honors a mid-window opt-out",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(1, "example.com"),
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()
	bgGenerateOne(t, leafURL)

	volKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, volKey, "BG02b OptOut Vol")
	wu, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volID, PublicKey: volKey})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	wuID := wu.Assignments[0].WorkUnitId
	ensureRunStart(t, env.pool, env.grpc, ctx, volID, volKey, wuID)
	sub, err := env.grpc.SubmitResult(signFor(t, ctx, volKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volID, PublicKey: volKey,
		OutputDataUrl: "https://example.com/results/optout.json", OutputChecksumSha256: sha256Hex(served),
		Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := bgReadResult(t, env, ctx, sub.ResultId).status; got != string(result.ValidationAwaitingContentVerification) {
		t.Fatalf("reference status = %q, want AWAITING_CONTENT_VERIFICATION", got)
	}

	// Opt out: clear allow_external_output AND the allowlist. Sent as raw JSON so the empty array
	// is transmitted explicitly — a typed struct with omitempty would drop it and the field-merge
	// would keep the old allowlist (which the validator then refuses as vestigial config).
	resp := httpReq(t, "PUT", leafURL, map[string]any{
		"validation_config": map[string]any{
			"allow_external_output": false,
			"external_output_hosts": []string{},
		},
	})
	requireStatus(t, resp, http.StatusOK, "opt out of external output")
	resp.Body.Close()

	// Worker tick → the fetch-time re-check sees the leaf no longer opted in → terminal
	// LEAF_OPTED_OUT, and the origin is never contacted.
	stop := bgStartWorker(ctx, bgContentWorker(env, client))
	bgWaitFor(t, 5*time.Second, "reference terminally failed on opt-out", func() bool {
		return bgReadResult(t, env, ctx, sub.ResultId).status == string(result.ValidationContentVerificationFailed)
	})
	stop()

	row := bgReadResult(t, env, ctx, sub.ResultId)
	if row.lastError == nil || !strings.HasPrefix(*row.lastError, contentverify.CodeLeafOptedOut) {
		t.Fatalf("last_error = %v, want prefix %s", row.lastError, contentverify.CodeLeafOptedOut)
	}
	if hits != 0 {
		t.Fatalf("origin was fetched %d times; a fetch-time opt-out must short-circuit BEFORE any fetch", hits)
	}
}

// --- (g) late promotion onto a finalized unit ---

// TestBG02b_g_LatePromotionUnitFinalized is §10.11(g). A reference is held, then the unit is
// forced VALIDATED out from under it (the late-validation race). On the tick the fetch may
// succeed, but the finalized-unit check turns the promotion into a terminal UNIT_FINALIZED — the
// seat is gone. The row must never become PENDING.
func TestBG02b_g_LatePromotionUnitFinalized(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	server.SetContentFetchPolicy(env.volunteerSvc, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bytesB := []byte(`{"late":true}`)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytesB)
	}))
	defer srv.Close()
	client := bgInjectedClient(t, srv)

	userID := createTestUser(t, env.pool, ctx, "bg02b-g")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b LatePromotion", Description: "unit finalized while the ref was held",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(1, "example.com"),
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()
	bgGenerateOne(t, leafURL)

	volKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, volKey, "BG02b Late Vol")
	wu, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volID, PublicKey: volKey})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	wuID := wu.Assignments[0].WorkUnitId
	ensureRunStart(t, env.pool, env.grpc, ctx, volID, volKey, wuID)
	sub, err := env.grpc.SubmitResult(signFor(t, ctx, volKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volID, PublicKey: volKey,
		OutputDataUrl: "https://example.com/results/late.json", OutputChecksumSha256: sha256Hex(bytesB),
		Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Force the unit VALIDATED out from under the held ref (the late-validation race).
	if _, err := env.pool.Exec(ctx,
		"UPDATE work_units SET state = 'VALIDATED', completed_at = COALESCE(completed_at, now()) WHERE id = $1", wuID); err != nil {
		t.Fatalf("force unit VALIDATED: %v", err)
	}

	// Worker tick → terminal UNIT_FINALIZED; the row must NEVER become PENDING.
	stop := bgStartWorker(ctx, bgContentWorker(env, client))
	bgWaitFor(t, 5*time.Second, "reference terminally failed as UNIT_FINALIZED", func() bool {
		return bgReadResult(t, env, ctx, sub.ResultId).status == string(result.ValidationContentVerificationFailed)
	})
	stop()

	row := bgReadResult(t, env, ctx, sub.ResultId)
	if row.status == string(result.ValidationPending) || row.status == string(result.ValidationAgreed) {
		t.Fatalf("late reference became %q; a finalized unit's seat is gone — it must never promote", row.status)
	}
	if row.lastError == nil || !strings.HasPrefix(*row.lastError, contentverify.CodeUnitFinalized) {
		t.Fatalf("last_error = %v, want prefix %s", row.lastError, contentverify.CodeUnitFinalized)
	}
}

// --- (h) default-off equivalence ---

// TestBG02b_h_DefaultOffEquivalence is §10.11(h). With the fetch knob DEFAULTED (never set — the
// deploy-safety off state), a reference submit on an opted-in leaf is refused at the front door
// (FailedPrecondition), and a pure-inline two-volunteer EXACT flow validates + credits exactly as
// before slice-5 — proving the default path is behavior-identical.
func TestBG02b_h_DefaultOffEquivalence(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	// Knob DEFAULTED — SetContentFetchPolicy is intentionally never called.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userID := createTestUser(t, env.pool, ctx, "bg02b-h")

	// Part 1: a reference submit on an OPTED-IN leaf is refused with the knob defaulted.
	optedIn := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b DefaultOff OptedIn", Description: "ref refused with the knob defaulted",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(1, "example.com"),
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	optedInURL := env.httpURL + "/api/v1/leafs/" + optedIn.ID.String()
	bgGenerateOne(t, optedInURL)
	refVolKey := []byte(genVolunteerKey(t))
	refVolID := registerVolunteer(t, env, ctx, refVolKey, "BG02b DefaultOff Ref Vol")
	refWU, err := env.grpc.RequestWorkUnit(signFor(t, ctx, refVolKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: refVolID, PublicKey: refVolKey})
	if err != nil {
		t.Fatalf("ref request: %v", err)
	}
	refWUID := refWU.Assignments[0].WorkUnitId
	ensureRunStart(t, env.pool, env.grpc, ctx, refVolID, refVolKey, refWUID)
	_, err = env.grpc.SubmitResult(signFor(t, ctx, refVolKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: refWUID, VolunteerId: refVolID, PublicKey: refVolKey,
		OutputDataUrl: "https://example.com/results/default-off.json", OutputChecksumSha256: strings.Repeat("ef", 32),
		Metadata: bgExecMeta(),
	})
	if err == nil {
		t.Fatal("reference submit accepted with the knob defaulted; want FailedPrecondition")
	}
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Fatalf("default-off ref submit code = %s, want FailedPrecondition", st.Code())
	}

	// Part 2: a pure-inline two-volunteer EXACT flow validates + credits, unchanged by slice-5.
	inlineLeaf := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b DefaultOff Inline", Description: "the inline path is intact",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		leaf.ValidationConfig{RedundancyFactor: 2, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	inlineURL := env.httpURL + "/api/v1/leafs/" + inlineLeaf.ID.String()
	bgGenerateOne(t, inlineURL)

	volAKey := []byte(genVolunteerKey(t))
	volBKey := []byte(genVolunteerKey(t))
	volAID := registerVolunteer(t, env, ctx, volAKey, "BG02b Inline A")
	volBID := registerVolunteer(t, env, ctx, volBKey, "BG02b Inline B")
	wuResp, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volAKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volAID, PublicKey: volAKey})
	if err != nil {
		t.Fatalf("A request: %v", err)
	}
	wuID := wuResp.Assignments[0].WorkUnitId
	runStartWU(t, env, ctx, wuID, volAID, volAKey)
	createRedundantAssignment(t, env.pool, ctx, wuID, types.MustParseID(volBID))

	inlineBytes := []byte(`{"inline":"default-off"}`)
	inlineCk := sha256Hex(inlineBytes)
	for _, v := range []struct {
		id  string
		key []byte
	}{{volAID, volAKey}, {volBID, volBKey}} {
		if _, err := env.grpc.SubmitResult(signFor(t, ctx, v.key), &lettucev1.SubmitResultRequest{
			WorkUnitId: wuID, VolunteerId: v.id, PublicKey: v.key,
			OutputData: inlineBytes, OutputChecksumSha256: inlineCk, Metadata: bgExecMeta(),
		}); err != nil {
			t.Fatalf("inline submit (%s): %v", v.id, err)
		}
	}
	if st := bgUnitState(t, env, ctx, wuID); st != "VALIDATED" {
		t.Fatalf("inline unit state = %q, want VALIDATED", st)
	}
	if n := bgCountCredit(t, env, ctx, "SELECT COUNT(*) FROM credit_ledger WHERE work_unit_id = $1", wuID); n != 2 {
		t.Fatalf("inline credit_ledger rows = %d, want 2", n)
	}
}

// --- (j) results API filter serves the two new states ---

// TestBG02b_j_ResultsAPIFilter is §10.11(j). While a reference is held, the results list endpoint
// filtered by AWAITING_CONTENT_VERIFICATION returns it; after the tick fails it (origin 404),
// the CONTENT_VERIFICATION_FAILED filter returns it. Both are served through the existing list
// route with no new admin surface.
func TestBG02b_j_ResultsAPIFilter(t *testing.T) {
	env, cleanup := setupAlphaServer(t)
	defer cleanup()
	server.SetContentFetchPolicy(env.volunteerSvc, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := bgInjectedClient(t, srv)

	userID := createTestUser(t, env.pool, ctx, "bg02b-j")
	proj := createAndActivateProject(t, env, ctx,
		leaf.CreateLeafRequest{
			Name: "BG02b ResultsFilter", Description: "results API serves the new validation states",
			ResearchArea: []string{"testing"}, TaskPattern: leaf.PatternParameterSweep,
			IsOngoing: false, Visibility: leaf.VisibilityPublic, CreatorID: &userID,
		},
		bgExternalOutputValConfig(1, "example.com"),
		leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	)
	leafURL := env.httpURL + "/api/v1/leafs/" + proj.ID.String()
	bgGenerateOne(t, leafURL)

	volKey := []byte(genVolunteerKey(t))
	volID := registerVolunteer(t, env, ctx, volKey, "BG02b Filter Vol")
	wu, err := env.grpc.RequestWorkUnit(signFor(t, ctx, volKey), &lettucev1.RequestWorkUnitRequest{VolunteerId: volID, PublicKey: volKey})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	wuID := wu.Assignments[0].WorkUnitId
	ensureRunStart(t, env.pool, env.grpc, ctx, volID, volKey, wuID)
	sub, err := env.grpc.SubmitResult(signFor(t, ctx, volKey), &lettucev1.SubmitResultRequest{
		WorkUnitId: wuID, VolunteerId: volID, PublicKey: volKey,
		OutputDataUrl: "https://example.com/results/filter.json", OutputChecksumSha256: strings.Repeat("12", 32),
		Metadata: bgExecMeta(),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// While held: the AWAITING_CONTENT_VERIFICATION filter returns the row.
	if !bgResultsFilterHas(t, leafURL, "AWAITING_CONTENT_VERIFICATION", sub.ResultId) {
		t.Fatal("held row not returned by ?validation_status=AWAITING_CONTENT_VERIFICATION")
	}

	// After the tick fails it: the CONTENT_VERIFICATION_FAILED filter returns the row.
	stop := bgStartWorker(ctx, bgContentWorker(env, client))
	bgWaitFor(t, 6*time.Second, "reference terminally failed", func() bool {
		return bgReadResult(t, env, ctx, sub.ResultId).status == string(result.ValidationContentVerificationFailed)
	})
	stop()
	if !bgResultsFilterHas(t, leafURL, "CONTENT_VERIFICATION_FAILED", sub.ResultId) {
		t.Fatal("failed row not returned by ?validation_status=CONTENT_VERIFICATION_FAILED")
	}
}
