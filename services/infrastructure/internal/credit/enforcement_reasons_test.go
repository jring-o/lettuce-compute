package credit

import (
	"regexp"
	"testing"
)

// signerReasonRe replicates the revocation signer's reason-code charset check VERBATIM
// (attestation/signer.go:76: `^[A-Z0-9_]{1,64}$`). It is duplicated here rather than imported
// because the signer's reasonRe is unexported. The two machine enforcement reason codes must
// satisfy it: EmitForAdjustment signs the revocation payload, and a non-conforming reason code
// would fail that signing at emit time. Pinning the property here makes such a code
// unrepresentable instead of a runtime surprise.
var signerReasonRe = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

func TestEnforcementReasonCodesMatchSignerCharset(t *testing.T) {
	for _, code := range []string{ReasonAuditMismatch, ReasonAuditMismatchUnmatured} {
		if !signerReasonRe.MatchString(code) {
			t.Errorf("reason code %q does not match the revocation signer charset %s (attestation/signer.go:76) — it would fail revocation signing at emit time",
				code, signerReasonRe.String())
		}
	}
}

// Compile-time assertions that the pgx repositories satisfy the slice-3 enforcement
// interfaces whose methods this package implements.
var (
	_ AdjustmentsRepository = (*PgxAdjustmentsRepository)(nil)
	_ RACAdjuster           = (*PgxRACRepository)(nil)
)
