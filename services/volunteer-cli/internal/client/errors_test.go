package client

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// headHostUnknownMessage reproduces the head's full host-unknown refusal text
// (server.HostUnknownMessage). It is hardcoded, not imported: the head constant lives
// under internal/ and is unimportable from this module. The word "outdated" in it is
// load-bearing — see the test below.
const headHostUnknownMessage = "unknown or revoked host id: this volunteer build is outdated — run 'lettuce-volunteer update' (updated builds re-register and acquire a fresh id automatically)"

// The host-unknown refusal is matched by BOTH IsHostUnknownError (the prefix) AND
// IsVolunteerTooOldError (the "outdated" substring) — by design (design audit F-G): the
// same message means "stop and update" to a pre-issuance build and "re-register and
// continue" to an issuance-aware build. The fetcher disambiguates by checking
// IsHostUnknownError FIRST. This test pins the dual-match so a future classifier edit
// can't silently reduce it to a too-old-only match (which would strand new builds in
// no-work mode); the fetcher's routing order is pinned separately in the daemon package.
func TestClassifiers_HostUnknownAlsoMatchesTooOld(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, headHostUnknownMessage)
	if !IsHostUnknownError(err) {
		t.Error("IsHostUnknownError = false, want true for the host-unknown refusal")
	}
	if !IsVolunteerTooOldError(err) {
		t.Error("IsVolunteerTooOldError = false, want true (the message contains 'outdated' by design)")
	}
	if IsPowRequiredError(err) || IsPowRejectedError(err) {
		t.Error("host-unknown refusal must not classify as a pow signal")
	}
}

// pow-required shares the FailedPrecondition code and the "outdated" hint (so old builds
// print the update hint) but a DIFFERENT prefix, so the two FailedPrecondition refusals
// never cross-classify.
func TestClassifiers_PowRequiredRefusal(t *testing.T) {
	err := status.Error(codes.FailedPrecondition,
		"registration requires proof-of-work: this volunteer build is outdated — run 'lettuce-volunteer update' to get a build that can solve registration challenges")
	if !IsPowRequiredError(err) {
		t.Error("IsPowRequiredError = false, want true")
	}
	if IsHostUnknownError(err) {
		t.Error("IsHostUnknownError = true, want false (different prefix)")
	}
}

// pow-rejected is InvalidArgument, which IsVolunteerTooOldError must NOT match — a
// rejected solution must never route to the update hint.
func TestClassifiers_PowRejectedNotTooOld(t *testing.T) {
	err := status.Error(codes.InvalidArgument,
		"registration proof-of-work rejected: registration proof-of-work solution does not meet the difficulty target")
	if !IsPowRejectedError(err) {
		t.Error("IsPowRejectedError = false, want true")
	}
	if IsVolunteerTooOldError(err) {
		t.Error("IsVolunteerTooOldError = true, want false (InvalidArgument pow-rejected must not route to the update hint)")
	}
	if IsHostUnknownError(err) || IsPowRequiredError(err) {
		t.Error("pow-rejected must not classify as host-unknown or pow-required")
	}
}

// nil and non-status errors classify as nothing.
func TestClassifiers_NilAndPlain(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain", errors.New("boom")},
	} {
		if IsHostUnknownError(tc.err) || IsPowRequiredError(tc.err) || IsPowRejectedError(tc.err) {
			t.Errorf("%s: expected no classification", tc.name)
		}
	}
}
