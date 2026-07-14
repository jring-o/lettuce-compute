package validation

import (
	"encoding/json"
	"fmt"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

// ValidateSubmittedOutput enforces at the submit door exactly what the leaf's own comparator
// will later require of an INLINE result's output_data — no more, no less. One malformed,
// empty, or non-finite output used to abort the whole comparison and park its unit COMPLETED
// forever (BG-21a); the comparator now degrades such a row to a non-agreeing singleton, and
// this gate additionally surfaces the problem to the submitting client immediately (a clean
// InvalidArgument at submit) instead of as a mystery DISAGREED at validation time.
//
// The rules mirror the comparator per mode, from the code as it actually parses:
//
//   - Every inline output must be non-empty, well-formed JSON. results.output_data is jsonb,
//     so malformed JSON could never be STORED anyway — it used to die as an opaque 500 at
//     INSERT; refusing it here is a cleaner error, not a new refusal. Refusing EMPTY output
//     is genuinely new: an empty inline output can never corroborate anything (EXACT keys it
//     to a non-grouping sentinel; NUMERIC_TOLERANCE errors on it).
//
//   - NUMERIC_TOLERANCE leaves flatten the output into float64 leaves at comparison time
//     (flattenOutput), so the door runs the SAME flatten under the leaf's configured
//     ignore_fields/compare_fields. A payload the comparator would choke on — a
//     float64-overflow numeric like 1e400, or an unsupported leaf type — is refused here
//     with the flatten's own error. Running it under the leaf's field filters matters: a
//     field the validation never reads (ignored, or outside compare_fields) can never
//     refuse an honest submission.
//
//   - EXACT leaves never flatten. Without ignore_fields the comparator keys on the raw
//     checksum and never parses the output; with ignore_fields it canonicalizes via a
//     UseNumber decoder that preserves any syntactically-valid numeric verbatim. Either
//     way well-formedness is the comparator's only demand, so it is the door's only
//     demand — the gate must not refuse a submission today's validation accepts.
//
// The caller applies this to INLINE submissions only. Ref-only submissions (external
// output_data_url) carry no inline bytes to check — they are held for content verification
// and key on the head-verified checksum (§10.8), a separate pipeline with its own gates.
func ValidateSubmittedOutput(vc leaf.ValidationConfig, output json.RawMessage) error {
	if len(output) == 0 {
		return fmt.Errorf("output_data must be non-empty JSON")
	}
	if !json.Valid(output) {
		return fmt.Errorf("output_data is not well-formed JSON")
	}
	if vc.ComparisonMode == leaf.ComparisonNumericTolerance {
		if _, err := flattenOutput(output, vc.IgnoreFields, vc.CompareFields); err != nil {
			return fmt.Errorf("output_data would fail this leaf's numeric validation: %w", err)
		}
	}
	return nil
}
