package validation

// SetFinalizationDecorateHook installs a TEST-ONLY stores decorator on a production
// FinalizationTxRunner (the value returned by NewPgxFinalizationTxRunner), so a test can wrap
// the tx-scoped stores — e.g. replace Credits with a repo whose Create/CreateCapped fails —
// INSIDE the real transaction. This is how the atomicity refutation test proves that a credit
// write failing mid-accept rolls the whole transaction back (marks and flip included), leaving
// the unit COMPLETED with every result still PENDING (BG-21c/★E1-1).
//
// It panics if r is not the production runner, which is a test bug the loud failure surfaces
// immediately. decorate == nil clears any previously installed hook.
func SetFinalizationDecorateHook(r FinalizationTxRunner, decorate func(FinalizationStores) FinalizationStores) {
	r.(*pgxFinalizationTxRunner).decorate = decorate
}
