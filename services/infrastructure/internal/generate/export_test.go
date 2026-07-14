package generate

import "github.com/lettuce-compute/infrastructure/internal/workunit"

// LoadCursorForTest exposes the unexported cursor decoder to external (generate_test) tests.
func LoadCursorForTest(raw []byte) *GenerationCursor {
	return loadCursor(raw)
}

// SetBatchSinkDecorateWU installs the TEST-ONLY in-transaction work-unit repo decorator on a
// production sink, so an external integration test can inject a failure inside the batch tx and
// prove the whole transaction rolls back atomically.
func SetBatchSinkDecorateWU(s *PgxBatchSink, f func(workunit.WorkUnitRepository) workunit.WorkUnitRepository) {
	s.decorateWU = f
}
