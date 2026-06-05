package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// seeder creates (idempotently) an ACTIVE leaf with work units on a running
// head via its HTTP admin API. It builds request bodies as plain JSON maps
// rather than importing the infrastructure leaf/workunit packages, because the
// simulator lives in the volunteer-cli module and cannot import
// github.com/lettuce-compute/infrastructure/internal/*.
//
// All write routes are gated behind the admin Bearer token
// (LETTUCE_ADMIN_API_KEY); the admin key resolves to an ADMIN-role user that
// bypasses per-leaf ownership checks, so one key can create AND configure the
// leaf.
type seeder struct {
	httpURL   string // e.g. http://127.0.0.1:8080
	adminKey  string
	creatorID string // optional users.id to own the seeded leaf
	client    *http.Client
}

func newSeeder(httpURL, adminKey, creatorID string) *seeder {
	return &seeder{
		httpURL:   strings.TrimRight(httpURL, "/"),
		adminKey:  adminKey,
		creatorID: creatorID,
		client:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *seeder) do(ctx context.Context, method, path string, body any) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.httpURL+path, rdr)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.adminKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.adminKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, respBody, nil
}

// leafByName returns the ID of an existing leaf with the given name, or "" if
// none exists. Used to make seeding idempotent on the --seed-leaf slug.
func (s *seeder) leafByName(ctx context.Context, name string) (string, error) {
	resp, body, err := s.do(ctx, http.MethodGet, "/api/v1/leafs?limit=200", nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list leafs: status %d: %s", resp.StatusCode, truncate(body))
	}
	// The list endpoint returns the paginated envelope {"data":[...]}; older/other
	// shapes used {"leafs":[...]} or a bare array. Handle all three. Match on
	// either name or slug so a leaf created under a deduplicated slug
	// (e.g. "swarm-test-2") is still resolvable by its requested name.
	var wrapper struct {
		Data  []leafSummary `json:"data"`
		Leafs []leafSummary `json:"leafs"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil {
		for _, lf := range append(wrapper.Data, wrapper.Leafs...) {
			if lf.Name == name || lf.Slug == name {
				return lf.ID, nil
			}
		}
		if len(wrapper.Data) > 0 || len(wrapper.Leafs) > 0 {
			return "", nil
		}
	}
	var arr []leafSummary
	if err := json.Unmarshal(body, &arr); err == nil {
		for _, lf := range arr {
			if lf.Name == name || lf.Slug == name {
				return lf.ID, nil
			}
		}
	}
	return "", nil
}

type leafSummary struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	State string `json:"state"`
}

// seedResult carries what the volunteers need to target the seeded leaf.
type seedResult struct {
	LeafID       string
	UnitsCreated int
	Reused       bool
}

// rscFpopsEst is stamped on the leaf's execution config so each dispatched
// WorkUnitAssignment carries rsc_fpops_est for hours-based buffer sizing. It is
// a small synthetic value; the simulator divides it by --sim-fpops to derive a
// pretend-compute time.
const seedRscFpopsEst = 1.0e9

// Seed ensures an active leaf named seedLeaf exists with at least seedUnits work
// units, creating and configuring it if absent. It is idempotent: a re-run with
// the same slug reuses the existing leaf and tops up work units toward
// seedUnits.
func (s *seeder) Seed(ctx context.Context, seedLeaf string, seedUnits int) (*seedResult, error) {
	existingID, err := s.leafByName(ctx, seedLeaf)
	if err != nil {
		return nil, err
	}

	if existingID != "" {
		// Reuse: top up work units toward the target.
		created, genErr := s.generateUnits(ctx, existingID, seedUnits)
		if genErr != nil {
			// A leaf with all units already generated may reject a duplicate
			// generate; treat as reused with zero new units.
			return &seedResult{LeafID: existingID, UnitsCreated: 0, Reused: true}, nil
		}
		return &seedResult{LeafID: existingID, UnitsCreated: created, Reused: true}, nil
	}

	leafID, err := s.createLeaf(ctx, seedLeaf)
	if err != nil {
		return nil, err
	}
	if err := s.configureLeaf(ctx, leafID); err != nil {
		return nil, err
	}
	if err := s.activateLeaf(ctx, leafID); err != nil {
		return nil, err
	}
	created, err := s.generateUnits(ctx, leafID, seedUnits)
	if err != nil {
		return nil, err
	}
	return &seedResult{LeafID: leafID, UnitsCreated: created, Reused: false}, nil
}

func (s *seeder) createLeaf(ctx context.Context, name string) (string, error) {
	// Leaf metadata validation requires a creator_id, and leafs.creator_id has a
	// foreign key to users(id), so it must reference a real user row. The HTTP
	// create handler reads creator_id straight from the request body (it does not
	// derive it from the admin Bearer identity), so callers must supply one. The
	// admin Bearer key still resolves to an ADMIN-role user that bypasses per-leaf
	// ownership on the subsequent configure/activate/generate calls.
	//
	// When --creator-id is not provided, fall back to a generated UUID; this works
	// only if that id happens to exist as a user, so the standalone-head recipe
	// passes the bootstrapped admin user's id explicitly.
	creatorID := s.creatorID
	if creatorID == "" {
		creatorID = uuid.NewString()
	}
	body := map[string]any{
		"name":          name,
		"description":   "swarm-sim load-test leaf",
		"research_area": []string{"load-testing"},
		"task_pattern":  "PARAMETER_SWEEP",
		"is_ongoing":    false,
		"visibility":    "PUBLIC",
		"creator_id":    creatorID,
	}
	resp, respBody, err := s.do(ctx, http.MethodPost, "/api/v1/leafs", body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create leaf: status %d: %s", resp.StatusCode, truncate(respBody))
	}
	var lf leafSummary
	if err := json.Unmarshal(respBody, &lf); err != nil {
		return "", fmt.Errorf("decode created leaf: %w", err)
	}
	return lf.ID, nil
}

func (s *seeder) configureLeaf(ctx context.Context, leafID string) error {
	base := "/api/v1/leafs/" + leafID

	// configure (server fills defaults).
	resp, respBody, err := s.do(ctx, http.MethodPost, base+"/configure", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("configure leaf: status %d: %s", resp.StatusCode, truncate(respBody))
	}

	// Update configs with a minimal, valid NATIVE execution spec. redundancy=1
	// so a single submitted result completes the unit (keeps dispatch flowing).
	update := map[string]any{
		"execution_config": map[string]any{
			"runtime": "NATIVE",
			"binaries": map[string]string{
				"linux-amd64": "https://example.com/bin/linux-amd64",
			},
			"binary_checksums": map[string]string{
				"linux-amd64": strings.Repeat("0", 64),
			},
			"max_memory_mb":   4096,
			"max_disk_mb":     10240,
			"max_cpu_seconds": 3600,
			"rsc_fpops_est":   seedRscFpopsEst,
		},
		"validation_config": map[string]any{
			"redundancy_factor":   1,
			"agreement_threshold": 1.0,
			"comparison_mode":     "EXACT",
			"max_retries":         3,
		},
		"fault_tolerance_config": map[string]any{
			// heartbeat fields are deprecated/inert (deadline-based reassignment
			// replaced per-task heartbeats); only deadline_multiplier and
			// max_reassignments are still used.
			"deadline_multiplier": 3.0,
			"max_reassignments":   3,
		},
		"data_config": map[string]any{
			"transfer_strategy":     "INLINE",
			"aggregation_format":    "JSON",
			"max_input_size_bytes":  1048576,
			"max_output_size_bytes": 104857600,
		},
		"credit_config": map[string]any{
			"credit_per_validated_work_unit": 1.0,
		},
	}
	resp, respBody, err = s.do(ctx, http.MethodPut, base, update)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update leaf configs: status %d: %s", resp.StatusCode, truncate(respBody))
	}
	return nil
}

func (s *seeder) activateLeaf(ctx context.Context, leafID string) error {
	resp, respBody, err := s.do(ctx, http.MethodPost, "/api/v1/leafs/"+leafID+"/activate", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("activate leaf: status %d: %s", resp.StatusCode, truncate(respBody))
	}
	return nil
}

// generateUnits creates count work units via the parameter-sweep generate
// endpoint and returns the number actually created.
func (s *seeder) generateUnits(ctx context.Context, leafID string, count int) (int, error) {
	params := make([]float64, count)
	for i := range params {
		params[i] = float64(i + 1)
	}
	body := map[string]any{
		"parameter_space": map[string]any{
			"x": params,
		},
	}
	resp, respBody, err := s.do(ctx, http.MethodPost, "/api/v1/leafs/"+leafID+"/work-units/generate", body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusAccepted {
		return 0, fmt.Errorf("generate work units: status %d: %s", resp.StatusCode, truncate(respBody))
	}
	var genResp struct {
		WorkUnitsCreated int `json:"work_units_created"`
	}
	if err := json.Unmarshal(respBody, &genResp); err != nil {
		return 0, fmt.Errorf("decode generate response: %w", err)
	}
	return genResp.WorkUnitsCreated, nil
}

func truncate(b []byte) string {
	const max = 300
	s := string(b)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
