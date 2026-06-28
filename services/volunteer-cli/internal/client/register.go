package client

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
)

// BuildRegistrationRequest assembles a RegisterVolunteerRequest from identity,
// hardware capabilities, and config.
//
// availableRuntimes, when non-empty, overrides cfg.AvailableRuntimes for what we
// advertise to the head. Callers pass the runtimes the volunteer can ACTUALLY
// run (e.g. derived from the live runtime registry) so a box that lists
// CONTAINER in config but has no working Docker/Podman doesn't get assigned
// container work it can only abandon. When empty, falls back to config.
func BuildRegistrationRequest(pub ed25519.PublicKey, hostID string, hw *lettucev1.HardwareCapabilities, cfg *config.Config, availableRuntimes ...string) *lettucev1.RegisterVolunteerRequest {
	runtimes := availableRuntimes
	if len(runtimes) == 0 {
		runtimes = cfg.AvailableRuntimes
	}
	hostname, _ := os.Hostname()
	return &lettucev1.RegisterVolunteerRequest{
		PublicKey:         pub,
		DisplayName:       hostname,
		Hardware:          hw,
		AvailableRuntimes: runtimes,
		SchedulingMode:    cfg.Scheduling.Mode,
		// Per-machine host id (TODO #19): the head records this machine's advertised
		// runtimes/hardware against it (fixing the flapping-row bug) and keys per-machine
		// metering on it. Empty => per-account fallback.
		HostId: hostID,
	}
}

// Register performs the full registration flow: detect hardware, build request,
// call RegisterVolunteer RPC, persist the returned volunteer ID to config.
//
// availableRuntimes (optional) advertises the runtimes actually available rather
// than cfg.AvailableRuntimes — see BuildRegistrationRequest.
func Register(ctx context.Context, client *Client, pub ed25519.PublicKey, hostID string, cfg *config.Config, configPath string, availableRuntimes ...string) (string, bool, error) {
	hw := DetectHardware(cfg)
	req := BuildRegistrationRequest(pub, hostID, hw, cfg, availableRuntimes...)

	// Ride out a rate-limited head: RegisterVolunteer is authenticated, so it is
	// subject to the head's post-auth per-pubkey limiter, and a register failure is
	// fatal at the call site (a single-head daemon exits with "could not connect to
	// any configured server"). Treat codes.ResourceExhausted as "slow down, keep
	// trying" — like the connect probe already does — instead of fatal (TODO #64).
	var resp *lettucev1.RegisterVolunteerResponse
	err := retryRPCOnRateLimit(ctx, client.logger, "register volunteer", func(ctx context.Context) error {
		var rpcErr error
		resp, rpcErr = client.RegisterVolunteer(ctx, req)
		return rpcErr
	})
	if err != nil {
		return "", false, fmt.Errorf("registering volunteer: %w", err)
	}

	cfg.VolunteerID = resp.VolunteerId
	if err := cfg.Save(configPath); err != nil {
		return resp.VolunteerId, resp.Registered, fmt.Errorf("saving config after registration: %w", err)
	}

	return resp.VolunteerId, resp.Registered, nil
}
