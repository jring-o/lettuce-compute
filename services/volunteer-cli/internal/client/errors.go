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
