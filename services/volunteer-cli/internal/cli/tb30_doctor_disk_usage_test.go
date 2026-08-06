package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// Regression tests for TB-30 (doctor's disk-usage figure omitted cached
// container images and printed "all checks passed" on a host the daemon had
// fully disk-gated) and the doctor half of TB-31 (the per-leaf verdict now
// shows the allowance-budget arithmetic the live gate enforces).

// stubMetricsAPI serves the given /api/v1/metrics body and writes the
// daemon.json that points doctor's managementGet at it, returning the data dir.
func stubMetricsAPI(t *testing.T, body map[string]any) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}

	dataDir := t.TempDir()
	info := fmt.Sprintf(`{"port":%d,"token":"test-token","pid":1,"started_at":""}`, port)
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.json"), []byte(info), 0o600); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}
	return dataDir
}

// TB-30: with the daemon running, doctor must quote the daemon's OWN usage
// figure — the data-dir tree plus cached container images, the number the
// fetch gate enforces — not its own workspace-only walk. The tester's host:
// doctor measured 41 MB while the daemon was refusing all work at 27,723 MB
// of a 30,720 MB allowance.
func TestTB30_DoctorQuotesTheDaemonsDiskUsage(t *testing.T) {
	dataDir := stubMetricsAPI(t, map[string]any{
		"disk_used_mb":      27723,
		"disk_allowance_mb": 30720,
		"disk_usage_known":  true,
	})
	// Give the data dir a real (tiny) footprint so a workspace-only walk would
	// produce a visibly different number.
	if err := os.WriteFile(filepath.Join(dataDir, "volunteer.log"), bytes.Repeat([]byte("x"), 2048), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	usedMB, known := checkDiskUsage(rep, dataDir, 30, true)

	if !known || usedMB != 27723 {
		t.Fatalf("checkDiskUsage = (%d, %v), want (27723, true) — the running daemon's enforced figure", usedMB, known)
	}
	if !strings.Contains(buf.String(), "27723") {
		t.Errorf("doctor must print the daemon's figure (27723 MB); got:\n%s", buf.String())
	}
	if rep.fails != 0 {
		t.Errorf("fails = %d, want 0 — under the allowance is not a failure", rep.fails)
	}
}

// TB-30: without the daemon, a container host's workspace-only figure is
// PARTIAL — the dominant term (cached images) is unmeasured — so it must be
// reported as a warning with the budget verdict unknown, never fed into an
// "all checks passed" summary.
func TestTB30_DoctorWithoutDaemonOnContainerHostRefusesPassVerdict(t *testing.T) {
	dataDir := t.TempDir() // no daemon.json — the daemon is down
	if err := os.WriteFile(filepath.Join(dataDir, "volunteer.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	_, known := checkDiskUsage(rep, dataDir, 30, true)

	if known {
		t.Error("known = true for a workspace-only figure on a container host — a partial figure must not feed budget verdicts")
	}
	if rep.warns != 1 {
		t.Errorf("warns = %d, want 1 — a partial figure must not read as a pass; report:\n%s", rep.warns, buf.String())
	}
	if !strings.Contains(buf.String(), "images") {
		t.Errorf("the warning must say what is missing (cached container images); got:\n%s", buf.String())
	}
}

// TB-30: on a host with no usable container engine the gate itself cannot
// count images either (unknown fails open), so the workspace figure IS the
// enforced one and stays an ordinary informational line.
func TestTB30_EnginelessHostWorkspaceFigureIsComplete(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "volunteer.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	rep := &doctorReport{w: &buf}
	_, known := checkDiskUsage(rep, dataDir, 30, false)

	if !known {
		t.Error("known = false on an engine-less host — the workspace figure is exactly what the gate enforces there")
	}
	if rep.warns != 0 || rep.fails != 0 {
		t.Errorf("warns=%d fails=%d, want 0/0; report:\n%s", rep.warns, rep.fails, buf.String())
	}
}

// TB-31 (doctor surfacing): the per-leaf note must show the allowance-budget
// arithmetic the live gate enforces — usage + need vs max_disk_gb — because
// that is the half that gated the tester's host while every free-space figure
// looked fine. The numbers are his exactly: 27,723 MB used, 10,240 MB need,
// 30,720 MB allowance, 60 GB genuinely free.
func TestTB31_DoctorNoteShowsBudgetArithmetic(t *testing.T) {
	req := leafRequirements{name: "beyblade-cpu", diskMB: 10240}
	caps := volunteerCaps{
		freeDataDirMB: 61440,
		maxDiskMB:     30720,
		lettuceUsedMB: 27723,
		usedMBKnown:   true,
	}

	note := localDiskFetchNote(req, caps)
	if note == "" {
		t.Fatal("no note although 27,723 used + 10,240 need exceeds the 30,720 MB allowance — the budget half of the gate is invisible again (TB-30/TB-31)")
	}
	for _, want := range []string{"27723", "10240", "max_disk_gb"} {
		if !strings.Contains(note, want) {
			t.Errorf("note must show the budget arithmetic (%s); got: %s", want, note)
		}
	}

	// Under the budget: no note.
	caps.lettuceUsedMB = 10000
	if note := localDiskFetchNote(req, caps); note != "" {
		t.Errorf("10,000 + 10,240 fits under 30,720, want no note; got: %s", note)
	}

	// A partial usage figure must not produce a budget verdict at all.
	caps.lettuceUsedMB, caps.usedMBKnown = 27723, false
	if note := localDiskFetchNote(req, caps); note != "" {
		t.Errorf("partial usage figure must not feed a budget verdict; got: %s", note)
	}
}

// TB-30: when every leaf the head would dispatch is blocked by this machine's
// local disk state, the head line must escalate to a warning — the volunteer
// fetches nothing, and "all checks passed" over that state is the exact filed
// failure ("doctor says healthy while the daemon refuses all work").
func TestTB30_AllEligibleLeafsFetchGatedEscalates(t *testing.T) {
	leafs := []*lettucev1.LeafInfo{
		{Id: "a", Slug: "beyblade-cpu", ResourceRequirements: &lettucev1.LeafResourceRequirements{MinDiskMb: 10240}},
	}
	caps := volunteerCaps{
		maxMemoryMB:   16384,
		maxDiskMB:     30720,
		freeDataDirMB: 61440,
		lettuceUsedMB: 27723,
		usedMBKnown:   true,
	}

	res := evaluateLeafEligibility(leafs, caps, trustingHead, nil)
	if res.eligible != 1 {
		t.Fatalf("eligible = %d, want 1 (the head WOULD dispatch this leaf)", res.eligible)
	}
	if !allEligibleFetchGated(res) {
		t.Fatal("every eligible leaf carries a local disk-gate note, but the head line would still read as a pass (TB-30)")
	}

	// With the budget healthy nothing is gated and no escalation happens.
	caps.lettuceUsedMB = 100
	if allEligibleFetchGated(evaluateLeafEligibility(leafs, caps, trustingHead, nil)) {
		t.Error("nothing is gated, must not escalate")
	}
	// No eligible leafs at all is the ineligibility story, not this one.
	if allEligibleFetchGated(eligibilityResult{}) {
		t.Error("zero eligible leafs must not escalate the fetch-gate warning")
	}
}

// TB-31 (doctor gate parity): the live gate substitutes a fallback need for a
// leaf that declares none, so doctor must apply the same fallback — staying
// silent made doctor and the daemon disagree on exactly the leafs TB-31 is
// about. The note says the need is assumed.
func TestTB31_DoctorAppliesUnknownNeedFallback(t *testing.T) {
	req := leafRequirements{name: "beyblade-cpu"} // no declared disk need
	caps := volunteerCaps{
		freeDataDirMB: 47969,
		maxDiskMB:     10240,
		lettuceUsedMB: 9000, // 9,000 + fallback > 10,240
		usedMBKnown:   true,
	}

	note := localDiskFetchNote(req, caps)
	if note == "" {
		t.Fatal("no note although usage + the fallback need exceeds the allowance — doctor diverges from the live gate on undeclared leafs")
	}
	if !strings.Contains(note, "assumed") {
		t.Errorf("the note must say the need is assumed, not declared; got: %s", note)
	}

	// The fixed fallback clears the default allowance with real usage (the
	// TB-31 arithmetic): 454 MB used, 10,240 MB allowance.
	caps.lettuceUsedMB = 454
	if note := localDiskFetchNote(req, caps); note != "" {
		t.Errorf("454 MB used + the fallback must clear the 10,240 MB default allowance (TB-31); got: %s", note)
	}
}
