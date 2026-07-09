//go:build integration

package internal_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/client"
	"github.com/lettuce-compute/volunteer-cli/internal/config"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
	"github.com/lettuce-compute/volunteer-cli/internal/identity"
	"github.com/lettuce-compute/volunteer-cli/internal/project"
	volruntime "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// testBinarySource is a Go program that reads input, computes sha256, writes output.
const testBinarySource = `package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"time"
)

func main() {
	time.Sleep(100 * time.Millisecond)
	var data []byte
	if f := os.Getenv("LETTUCE_INPUT_FILE"); f != "" {
		d, _ := os.ReadFile(f)
		data = append(data, d...)
	}
	if f := os.Getenv("LETTUCE_PARAMS_FILE"); f != "" {
		d, _ := os.ReadFile(f)
		data = append(data, d...)
	}
	h := sha256.Sum256(data)
	out := hex.EncodeToString(h[:])
	outFile := os.Getenv("LETTUCE_OUTPUT_FILE")
	if outFile == "" {
		outFile = "output.dat"
	}
	os.WriteFile(outFile, []byte(out), 0644)
}
`

// TestE2EV04FullLifecycle tests the complete v0.4 volunteer lifecycle.
func TestE2EV04FullLifecycle(t *testing.T) {
	dbURL := os.Getenv("LETTUCE_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("LETTUCE_TEST_DB_URL not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tmpDir := t.TempDir()

	// ---- Build test binary ----
	testBinPath := buildTestBinary(t, tmpDir)

	// ---- Serve test binary via HTTP ----
	binSrv := newBinServer(t, testBinPath)
	defer binSrv.Close()

	pk := runtime.GOOS + "_" + runtime.GOARCH
	binaryURL := binSrv.URL + "/test-binary"

	// C2: the native runtime fails closed if the leaf does not carry a SHA-256
	// checksum for the platform's binary; compute it from the just-built binary.
	binBytes, err := os.ReadFile(testBinPath)
	if err != nil {
		t.Fatalf("read test binary for checksum: %v", err)
	}
	binSum := sha256.Sum256(binBytes)
	binaryChecksum := hex.EncodeToString(binSum[:])

	// ---- Build and start infrastructure server ----
	httpPort, grpcPort := findFreePorts(t)
	serverCmd := startInfraServer(t, tmpDir, dbURL, httpPort, grpcPort)
	defer func() {
		serverCmd.Process.Signal(os.Interrupt)
		serverCmd.Wait()
	}()

	httpBase := fmt.Sprintf("http://127.0.0.1:%d", httpPort)
	grpcAddr := fmt.Sprintf("127.0.0.1:%d", grpcPort)
	waitForHealth(t, ctx, httpBase)

	// ---- Set up test user in DB ----
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting to test DB: %v", err)
	}
	defer pool.Close()

	userID := uuid.New().String()
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, email, username, display_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (email) DO NOTHING`,
		userID,
		fmt.Sprintf("e2e-v04-%s@test.example.com", uuid.New().String()[:8]),
		fmt.Sprintf("e2e-v04-%s", uuid.New().String()[:8]),
		"E2E V04 Test User",
		"$argon2id$v=19$m=65536,t=3,p=4$fakesalt$fakehash",
	)
	if err != nil {
		t.Fatalf("creating test user: %v", err)
	}

	// ---- Create leafs ----
	leafAID := createTestLeaf(t, httpBase, userID, "E2E V04 Leaf A", binaryURL, binaryChecksum, pk, 3)
	t.Logf("Leaf A: %s", leafAID)

	leafBID := createTestLeaf(t, httpBase, userID, "E2E V04 Leaf B", binaryURL, binaryChecksum, pk, 1)
	t.Logf("Leaf B: %s", leafBID)

	// ---- Volunteer setup ----
	volDir := filepath.Join(tmpDir, "volunteer")
	os.MkdirAll(volDir, 0700)
	cfgPath := filepath.Join(volDir, "config.yaml")

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	cfg := config.Defaults()
	cfg.DataDir = volDir
	cfg.ResourceLimits.MaxCPUCores = 2
	cfg.ResourceLimits.MaxMemoryMB = 512
	cfg.ResourceLimits.MaxDiskGB = 5
	cfg.Servers = []config.ServerConfig{{
		GRPCAddress: grpcAddr,
		HTTPAddress: httpBase,
		Name:        "test-server",
	}}
	cfg.Save(cfgPath)

	// ---- Connect and register ----
	grpcClient, err := client.ConnectWithRetry(ctx, client.ClientConfig{
		ServerURL: grpcAddr,
		Insecure:  true,
		Identity:  &client.Identity{PublicKey: pub, PrivateKey: priv},
	}, client.RetryConfig{MaxRetries: 5}, logger)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer grpcClient.Close()

	// Host identity is head-issued (BG-25): register with an empty per-head store so the
	// head mints an id, then present exactly that id on subsequent work requests.
	hostStore := identity.NewHostIDStore(filepath.Join(volDir, "host-ids.json"))
	volID, _, hostID, err := client.Register(ctx, grpcClient, pub, hostStore, grpcAddr, cfg, cfgPath)
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	t.Logf("Volunteer: %s", volID)

	nativeRT := volruntime.NewNativeRuntime(volDir, logger)

	// ========== Scenario 1: Basic Lifecycle ==========
	t.Run("Scenario1_BasicLifecycle", func(t *testing.T) {
		wuResp, err := grpcClient.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID,
			PublicKey:   pub,
			HostId:      hostID,
		})
		if err != nil {
			t.Fatalf("RequestWorkUnit: %v", err)
		}
		if len(wuResp.Assignments) != 1 {
			t.Fatalf("expected 1 assignment, got %d", len(wuResp.Assignments))
		}
		wu := volruntime.WorkUnitFromProto(wuResp.Assignments[0])
		t.Logf("Received WU: %s (leaf: %s)", wu.ID, wu.LeafID)

		prep, err := nativeRT.Prepare(ctx, wu)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer nativeRT.Cleanup(prep)

		result, err := nativeRT.Execute(ctx, wu, prep)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("exit code = %d", result.ExitCode)
		}
		if len(result.OutputData) == 0 {
			t.Fatal("empty output")
		}
		if result.Metrics.WallClockSeconds <= 0 {
			t.Error("wall clock should be > 0")
		}

		submitResp, err := grpcClient.SubmitResult(ctx, &lettucev1.SubmitResultRequest{
			WorkUnitId:           wu.ID,
			VolunteerId:          volID,
			PublicKey:            pub,
			OutputData:           result.OutputData,
			OutputChecksumSha256: result.OutputChecksum,
			Metadata:             volruntime.MetricsToProto(&result.Metrics),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		if !submitResp.Accepted {
			t.Error("result not accepted")
		}

		daemon.AppendHistory(volDir, daemon.HistoryEntry{
			WorkUnitID:       wu.ID,
			LeafID:           wu.LeafID,
			CompletedAt:      time.Now().UTC(),
			WallClockSeconds: result.Metrics.WallClockSeconds,
			ResultAccepted:   submitResp.Accepted,
		})
	})

	// ========== Scenario 2: Self-Hosted Leaf ==========
	t.Run("Scenario3_SelfHosted", func(t *testing.T) {
		wuResp, err := grpcClient.RequestWorkUnit(ctx, &lettucev1.RequestWorkUnitRequest{
			VolunteerId: volID,
			PublicKey:   pub,
			HostId:      hostID,
		})
		if err != nil {
			t.Log("No remaining work units (all consumed)")
			return
		}
		if len(wuResp.Assignments) == 0 {
			t.Log("No remaining work units (all consumed)")
			return
		}
		wu := volruntime.WorkUnitFromProto(wuResp.Assignments[0])
		t.Logf("Self-hosted WU: %s (leaf: %s)", wu.ID, wu.LeafID)

		prep, err := nativeRT.Prepare(ctx, wu)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer nativeRT.Cleanup(prep)

		result, err := nativeRT.Execute(ctx, wu, prep)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}

		submitResp, err := grpcClient.SubmitResult(ctx, &lettucev1.SubmitResultRequest{
			WorkUnitId:           wu.ID,
			VolunteerId:          volID,
			PublicKey:            pub,
			OutputData:           result.OutputData,
			OutputChecksumSha256: result.OutputChecksum,
			Metadata:             volruntime.MetricsToProto(&result.Metrics),
		})
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		if !submitResp.Accepted {
			t.Error("result not accepted")
		}

		daemon.AppendHistory(volDir, daemon.HistoryEntry{
			WorkUnitID:       wu.ID,
			LeafID:           wu.LeafID,
			CompletedAt:      time.Now().UTC(),
			WallClockSeconds: result.Metrics.WallClockSeconds,
			ResultAccepted:   submitResp.Accepted,
		})
	})

	// ========== Scenario 4: Status and History ==========
	t.Run("Scenario4_StatusAndHistory", func(t *testing.T) {
		mgr := project.NewManager(cfg, cfgPath, logger)

		st, err := mgr.GetStatus(ctx)
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		if st.VolunteerID == "" {
			t.Error("empty volunteer ID in status")
		}
		if len(st.Servers) == 0 {
			t.Error("no servers in status")
		}

		entries, err := mgr.GetHistory(ctx, 50)
		if err != nil {
			t.Fatalf("GetHistory: %v", err)
		}
		t.Logf("Total history entries: %d", len(entries))
		if len(entries) == 0 {
			t.Fatal("no history entries")
		}
		for i, e := range entries {
			if e.WorkUnitID == "" {
				t.Errorf("entry %d: empty work_unit_id", i)
			}
			if !e.ResultAccepted {
				t.Errorf("entry %d: not accepted", i)
			}
		}
	})

	// ========== Scenario 5: Graceful Shutdown ==========
	t.Run("Scenario5_GracefulShutdown", func(t *testing.T) {
		// Create a fresh leaf with 1 work unit.
		freshID := createTestLeaf(t, httpBase, userID, "E2E V04 Shutdown", binaryURL, binaryChecksum, pk, 1)
		t.Logf("Shutdown test leaf: %s", freshID)

		d := daemon.NewDaemon(daemon.DaemonConfig{
			Config:      cfg,
			PubKey:      pub,
			PrivKey:     priv,
			Client:      grpcClient,
			Runtime:     nativeRT,
			VolunteerID: volID,
			Logger:      logger,
		})
		d.SetBackoff(10*time.Millisecond, 100*time.Millisecond)

		shutCtx, shutCancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- d.Run(shutCtx) }()

		// Let daemon pick up work, then cancel.
		time.Sleep(500 * time.Millisecond)
		shutCancel()

		select {
		case <-done:
			t.Log("Daemon stopped gracefully")
		case <-time.After(15 * time.Second):
			t.Fatal("daemon did not stop within timeout")
		}
	})
}

// --- Helpers ---

func buildTestBinary(t *testing.T, tmpDir string) string {
	t.Helper()
	srcDir := filepath.Join(tmpDir, "test-binary-src")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(testBinarySource), 0644)
	os.WriteFile(filepath.Join(srcDir, "go.mod"),
		[]byte(fmt.Sprintf("module test-binary\n\ngo %s\n", goVer())), 0644)

	binName := "test-binary"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)

	cmd := exec.Command(goExe(), "build", "-o", binPath, ".")
	cmd.Dir = srcDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building test binary: %v", err)
	}
	return binPath
}

type binServer struct {
	URL    string
	closer func()
}

func (s *binServer) Close() { s.closer() }

func newBinServer(t *testing.T, binPath string) *binServer {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/test-binary", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, binPath)
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	port := l.Addr().(*net.TCPAddr).Port
	return &binServer{
		URL:    fmt.Sprintf("http://127.0.0.1:%d", port),
		closer: func() { srv.Close() },
	}
}

func startInfraServer(t *testing.T, tmpDir, dbURL string, httpPort, grpcPort int) *exec.Cmd {
	t.Helper()
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse DB URL: %v", err)
	}
	dbHost := parsed.Hostname()
	dbPort := parsed.Port()
	if dbPort == "" {
		dbPort = "5432"
	}
	dbName := strings.TrimPrefix(parsed.Path, "/")
	dbUser := parsed.User.Username()
	dbPass, _ := parsed.User.Password()
	sslMode := parsed.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "disable"
	}

	// Locate infrastructure directory.
	wd, _ := os.Getwd()
	infraDir := filepath.Join(wd, "..", "..", "infrastructure")
	if _, err := os.Stat(infraDir); err != nil {
		infraDir = filepath.Join(wd, "..", "infrastructure")
	}

	serverBin := filepath.Join(tmpDir, "lettuce-server")
	if runtime.GOOS == "windows" {
		serverBin += ".exe"
	}

	buildCmd := exec.Command(goExe(), "build", "-o", serverBin, "./cmd/lettuce-server/")
	buildCmd.Dir = infraDir
	buildCmd.Stdout = os.Stderr
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("building server: %v", err)
	}

	// head.name is a required config field (validated at startup), so it is supplied.
	cfgContent := fmt.Sprintf(`head:
  name: "test-head"
server:
  http_addr: ":%d"
  grpc_addr: ":%d"
database:
  host: "%s"
  port: %s
  database: "%s"
  user: "%s"
  password: "%s"
  ssl_mode: "%s"
  max_conns: 5
  min_conns: 1
log:
  level: "debug"
  format: "json"
`, httpPort, grpcPort, dbHost, dbPort, dbName, dbUser, dbPass, sslMode)

	cfgPath := filepath.Join(tmpDir, "lettuce.yaml")
	os.WriteFile(cfgPath, []byte(cfgContent), 0644)

	cmd := exec.Command(serverBin, "-config", cfgPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// The server enforces several startup security requirements; supply test-only
	// values: a throwaway admin API key, and the dev escape hatch to auto-generate an
	// ephemeral Ed25519 signing key (it otherwise refuses to start without a
	// persistent key file).
	cmd.Env = append(os.Environ(),
		"LETTUCE_ADMIN_API_KEY=test-admin-key-e2e-v04",
		"LETTUCE_SIGNING_KEY_AUTOGEN=true",
		// Relax the binary URL SSRF rules so leafs whose binary URL points at the
		// local httptest server (http loopback) pass activation validation.
		"LETTUCE_BINARY_URL_ALLOW_INSECURE=true",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	return cmd
}

func waitForHealth(t *testing.T, ctx context.Context, base string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("server not healthy within 30s")
		case <-ctx.Done():
			t.Fatal("context done waiting for health")
		case <-ticker.C:
			resp, err := http.Get(base + "/api/v1/health")
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}
}

func createTestLeaf(t *testing.T, base, userID, name, binURL, binChecksum, platformKey string, wuCount int) string {
	t.Helper()

	createBody := map[string]interface{}{
		"name":          name,
		"description":   "E2E V04 test",
		"research_area": []string{"distributed-computing"},
		"task_pattern":  "PARAMETER_SWEEP",
		"visibility":    "PUBLIC",
		"creator_id":    userID,
	}
	resp := restReq(t, "POST", base+"/api/v1/leafs", createBody)
	requireHTTP(t, resp, http.StatusCreated, "create")
	var p map[string]interface{}
	decJSON(t, resp, &p)
	pid := p["id"].(string)
	pURL := base + "/api/v1/leafs/" + pid

	resp = restReq(t, "POST", pURL+"/configure", nil)
	requireHTTP(t, resp, http.StatusOK, "configure")
	resp.Body.Close()

	params := make([]interface{}, wuCount)
	for i := range params {
		params[i] = float64(i + 1)
	}

	update := map[string]interface{}{
		"execution_config": map[string]interface{}{
			"runtime":          "NATIVE",
			"binaries":         map[string]string{platformKey: binURL},
			"binary_checksums": map[string]string{platformKey: binChecksum},
			"gpu_type":         "ANY",
			"max_memory_mb":    4096,
			"max_disk_mb":      10240,
			"max_cpu_seconds":  3600,
		},
		"validation_config": map[string]interface{}{
			"redundancy_factor":   1,
			"agreement_threshold": 1.0,
			"comparison_mode":     "EXACT",
			"max_retries":         3,
		},
		"fault_tolerance_config": map[string]interface{}{
			"heartbeat_interval_seconds":  300,
			"missed_heartbeats_threshold": 3,
			"deadline_multiplier":         3.0,
			"max_reassignments":           3,
		},
		"data_config": map[string]interface{}{
			"transfer_strategy":     "INLINE",
			"aggregation_format":    "JSON",
			"max_input_size_bytes":  1048576,
			"max_output_size_bytes": 104857600,
			"splitting_config":      map[string]interface{}{"x": params},
		},
	}
	resp = restReq(t, "PUT", pURL, update)
	requireHTTP(t, resp, http.StatusOK, "update")
	resp.Body.Close()

	resp = restReq(t, "POST", pURL+"/activate", nil)
	requireHTTP(t, resp, http.StatusOK, "activate")
	resp.Body.Close()

	gen := map[string]interface{}{
		"parameter_space": map[string]interface{}{"x": params},
	}
	resp = restReq(t, "POST", pURL+"/work-units/generate", gen)
	requireHTTP(t, resp, http.StatusAccepted, "generate")
	var gr map[string]interface{}
	decJSON(t, resp, &gr)
	if int(gr["work_units_created"].(float64)) != wuCount {
		t.Fatalf("created %v, want %d", gr["work_units_created"], wuCount)
	}

	return pid
}

// testAdminAPIKey must match the LETTUCE_ADMIN_API_KEY env var passed to the
// infra server in startInfraServer. The server's authMiddleware accepts this as
// an ADMIN bearer token, satisfying the authOnly/authOwner wrappers on mutating
// /api/v1/leafs routes (HTTP route protection added alongside C1's gRPC auth).
const testAdminAPIKey = "test-admin-key-e2e-v04"

func restReq(t *testing.T, method, url string, body interface{}) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// All requests carry the admin bearer token; read endpoints ignore it harmlessly,
	// mutating endpoints (POST /api/v1/leafs, /activate, /configure, /work-units/...)
	// require it now that the production router wraps them with authOnly/authOwner.
	req.Header.Set("Authorization", "Bearer "+testAdminAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func requireHTTP(t *testing.T, resp *http.Response, want int, step string) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status %d, want %d: %s", step, resp.StatusCode, want, b)
	}
}

func decJSON(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(v)
}

func findFreePorts(t *testing.T) (int, int) {
	t.Helper()
	var ports [2]int
	for i := range ports {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		ports[i] = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	return ports[0], ports[1]
}

func goExe() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

func goVer() string {
	v := strings.TrimPrefix(runtime.Version(), "go")
	parts := strings.Split(v, ".")
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// expectedHash computes what the test binary would output for given input/params.
func expectedHash(input, params []byte) string {
	var data []byte
	data = append(data, input...)
	data = append(data, params...)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
