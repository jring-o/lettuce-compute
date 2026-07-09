package client

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// IsVolunteerTooOldError reports whether err is a gRPC status that looks like the
// head rejecting this volunteer's build as too old for it — the protocol-version
// coupling that rejects out-of-date volunteers fleet-wide. The head's explicit
// rejection carries a "too old" message, but we also treat an
// Unauthenticated/FailedPrecondition whose message references a version/build
// mismatch as the same class, so callers can surface a single actionable
// "run update" hint instead of a generic transport error. Kept conservative so a
// routine signature/auth failure is not mis-classified as a version problem.
func IsVolunteerTooOldError(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	msg := strings.ToLower(st.Message())
	if strings.Contains(msg, "too old") || strings.Contains(msg, "outdated") {
		return true
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.FailedPrecondition:
		return strings.Contains(msg, "version") &&
			(strings.Contains(msg, "mismatch") ||
				strings.Contains(msg, "old") ||
				strings.Contains(msg, "update") ||
				strings.Contains(msg, "unsupported") ||
				strings.Contains(msg, "incompatible"))
	}
	return false
}

// The three constants below are the CLI-side twins of head-side message prefixes.
// We pin our own copies rather than importing the head packages: those prefixes live
// under internal/ (unimportable from this separate module) and the CLI must not depend
// on head internals. If a head-side prefix ever changes, its twin here must change in
// lockstep — the head comments name this obligation, and the CLI carries a
// golden-vector import test so the two builds cannot silently diverge on the PoW rule.

// hostUnknownRefusalPrefix is the twin of server.HostUnknownMessagePrefix. The head
// refuses a RequestWorkUnit whose non-empty host id it did not issue to this account
// with FailedPrecondition and a message beginning with this prefix. The full message
// also contains the word "outdated", so PRE-issuance builds classify it via
// IsVolunteerTooOldError and print the update hint; issuance-aware builds MUST check
// IsHostUnknownError FIRST (see the fetcher's classification order) to route it to
// discard-id-and-re-register instead of the no-work update path.
const hostUnknownRefusalPrefix = "unknown or revoked host id"

// powRequiredRefusalPrefix is the twin of admission.PowRequiredMessagePrefix. When the
// head enforces registration proof-of-work, a brand-new key's RegisterVolunteer is
// refused with FailedPrecondition and a message beginning with this prefix until it
// carries a valid solution.
const powRequiredRefusalPrefix = "registration requires proof-of-work"

// powRejectedPrefix is the twin of the head's "registration proof-of-work rejected:
// ..." reply. After the client submits a solution the head cannot accept (stale/foreign
// challenge, or a nonce missing the difficulty target) the head answers InvalidArgument
// with a message beginning with this prefix; the client fetches a FRESH challenge and
// retries once.
const powRejectedPrefix = "registration proof-of-work rejected"

// IsHostUnknownError reports whether err is the head's host-unknown work-path refusal
// (FailedPrecondition + hostUnknownRefusalPrefix): the head did not issue this host id
// to this account (a head reset, a mint-time eviction, or an operator revocation). The
// caller discards the stored id for that head and re-registers to acquire a fresh one.
func IsHostUnknownError(err error) bool {
	return isFailedPreconditionWithPrefix(err, hostUnknownRefusalPrefix)
}

// IsPowRequiredError reports whether err is the head's pow-required registration
// refusal (FailedPrecondition + powRequiredRefusalPrefix): the caller must fetch a
// challenge, solve it, and retry registration with the solution.
func IsPowRequiredError(err error) bool {
	return isFailedPreconditionWithPrefix(err, powRequiredRefusalPrefix)
}

// IsPowRejectedError reports whether err is the head's pow-solution-rejected reply
// (InvalidArgument + powRejectedPrefix): the submitted solution was stale or wrong, so
// the caller fetches a fresh challenge and retries. InvalidArgument keeps this
// deliberately distinct from IsVolunteerTooOldError, so a rejected solution never
// routes to the "run update" hint.
func IsPowRejectedError(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	return st.Code() == codes.InvalidArgument && strings.HasPrefix(st.Message(), powRejectedPrefix)
}

// isFailedPreconditionWithPrefix is the shared shape of the two machine-readable
// FailedPrecondition refusals the head sends solver/issuance-aware clients.
func isFailedPreconditionWithPrefix(err error, prefix string) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	return st.Code() == codes.FailedPrecondition && strings.HasPrefix(st.Message(), prefix)
}
