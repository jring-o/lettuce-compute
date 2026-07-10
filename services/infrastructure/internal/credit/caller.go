package credit

import "context"

// Caller carries the authenticated caller's authorization facts, injected into the
// request context by the server router AFTER authentication, so the credit admin
// endpoints can enforce operator-only access WITHOUT importing the server package
// (import cycle). This mirrors trust.Caller / standing.Caller exactly: the router's
// authAdmin wrapper must inject this alongside those two — a handler wrapped in
// authAdmin WITHOUT the matching in-handler admin check is fail-open, and a handler
// checking a caller the router never injects is fail-closed-broken.
type Caller struct {
	IsAdmin bool
}

type callerContextKey struct{}

// WithCaller injects the authenticated caller's authorization facts into ctx.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, c)
}

// callerFromContext extracts the injected caller. Absent caller (no WithCaller
// upstream) yields the zero value — IsAdmin=false — so the credit admin API fails
// closed.
func callerFromContext(ctx context.Context) Caller {
	c, _ := ctx.Value(callerContextKey{}).(Caller)
	return c
}
