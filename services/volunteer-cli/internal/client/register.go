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
func BuildRegistrationRequest(pub ed25519.PublicKey, hw *lettucev1.HardwareCapabilities, cfg *config.Config) *lettucev1.RegisterVolunteerRequest {
	hostname, _ := os.Hostname()
	return &lettucev1.RegisterVolunteerRequest{
		PublicKey:         pub,
		DisplayName:       hostname,
		Hardware:          hw,
		AvailableRuntimes: cfg.AvailableRuntimes,
		SchedulingMode:    cfg.Scheduling.Mode,
	}
}

// Register performs the full registration flow: detect hardware, build request,
// call RegisterVolunteer RPC, persist the returned volunteer ID to config.
func Register(ctx context.Context, client *Client, pub ed25519.PublicKey, cfg *config.Config, configPath string) (string, bool, error) {
	hw := DetectHardware(cfg)
	req := BuildRegistrationRequest(pub, hw, cfg)

	resp, err := client.RegisterVolunteer(ctx, req)
	if err != nil {
		return "", false, fmt.Errorf("registering volunteer: %w", err)
	}

	cfg.VolunteerID = resp.VolunteerId
	if err := cfg.Save(configPath); err != nil {
		return resp.VolunteerId, resp.Registered, fmt.Errorf("saving config after registration: %w", err)
	}

	return resp.VolunteerId, resp.Registered, nil
}
