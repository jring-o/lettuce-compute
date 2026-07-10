package client

import (
	"context"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
)

// ClaimAuditJob calls the AuditService.ClaimJob RPC. The head derives the caller's
// identity from the signed request (this client's Ed25519 key) and returns the next
// queued audit job this runner's hardware class is eligible for, or a response with a
// nil Job when nothing is claimable. The request carries only the hardware
// capabilities; the head composes the hardware-redundancy class server-side. Only an
// ACTIVE registered runner is served — a caller that is not one gets PermissionDenied.
func (c *Client) ClaimAuditJob(ctx context.Context, req *lettucev1.ClaimAuditJobRequest) (*lettucev1.ClaimAuditJobResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.auditSvc.ClaimJob(ctx, req)
}

// SubmitAuditResult calls the AuditService.SubmitResult RPC, returning the re-executed
// output bytes (or an execution failure) for a claimed audit job. The head computes the
// checksum and adjudicates server-side — a runner never self-adjudicates, so the request
// carries no checksum or verdict field. Submitting a job that is no longer CLAIMED by
// this runner (completed or reclaimed) fails with FailedPrecondition; callers treat that
// as job-done rather than retrying.
func (c *Client) SubmitAuditResult(ctx context.Context, req *lettucev1.SubmitAuditResultRequest) (*lettucev1.SubmitAuditResultResponse, error) {
	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	return c.auditSvc.SubmitResult(ctx, req)
}
