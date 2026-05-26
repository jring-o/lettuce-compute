//go:build integration

package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/leaf"
)

// ---------- Viz tarball helpers ----------

// createTestVizTarGz creates a .tar.gz in tmpDir containing an index.html and other files.
// Returns the file path. The tarball has a single root directory "viz/".
func createTestVizTarGz(t *testing.T, tmpDir string, files map[string]string) string {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		entryName := "viz/" + name
		hdr := &tar.Header{
			Name: entryName,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	path := filepath.Join(tmpDir, "test-viz-bundle.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	return path
}

// serveVizTarball starts an HTTP server that serves a tarball at /viz-bundle.tar.gz.
// Returns the base URL and a cleanup function.
func serveVizTarball(t *testing.T, tarballPath string) (string, func()) {
	t.Helper()

	data, err := os.ReadFile(tarballPath)
	if err != nil {
		t.Fatalf("read tarball: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /viz-bundle.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(data)
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(lis) }()

	baseURL := "http://" + lis.Addr().String()
	return baseURL, func() { srv.Close() }
}

// ---------- Viz bundle extraction (desktop-side, tested at unit level) ----------

// extractVizBundleForTest extracts a tarball into a temp work directory.
// This mirrors the volunteer-cli's ExtractVizBundle logic.
func extractVizBundleForTest(t *testing.T, tarballPath, workDir string) string {
	t.Helper()

	destDir := filepath.Join(workDir, ".lettuce-viz")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("create viz dir: %v", err)
	}

	f, err := os.Open(tarballPath)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tarball: %v", err)
		}

		name := filepath.Clean(header.Name)
		name = filepath.ToSlash(name)

		// Strip leading directory component.
		parts := strings.SplitN(name, "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue
		}
		relPath := parts[1]
		if strings.Contains(relPath, "..") {
			t.Fatalf("path traversal in tarball: %s", header.Name)
		}

		target := filepath.Join(destDir, filepath.FromSlash(relPath))
		switch header.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			out, err := os.Create(target)
			if err != nil {
				t.Fatalf("create file: %v", err)
			}
			io.Copy(out, tr)
			out.Close()
		}
	}

	return destDir
}

// vizReadFile simulates the desktop app's viz_read_file command with path traversal protection.
func vizReadFile(vizDir, workDir, requestedPath string) ([]byte, error) {
	// Reject path traversal.
	if strings.Contains(requestedPath, "..") {
		return nil, fmt.Errorf("path traversal rejected: %s", requestedPath)
	}
	if filepath.IsAbs(requestedPath) {
		return nil, fmt.Errorf("absolute path rejected: %s", requestedPath)
	}

	fullPath := filepath.Join(workDir, requestedPath)
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	absWork, _ := filepath.Abs(workDir)
	if !strings.HasPrefix(absPath, absWork+string(filepath.Separator)) && absPath != absWork {
		return nil, fmt.Errorf("path escape: %s is outside %s", absPath, absWork)
	}

	return os.ReadFile(fullPath)
}

// downloadCacheAndExtract downloads a viz bundle, caches it, and extracts to a work directory.
// Returns the extracted viz directory path.
func downloadCacheAndExtract(t *testing.T, vizBundleURL string) (vizPath string, workDir string) {
	t.Helper()

	dataDir := t.TempDir()
	cacheDir := filepath.Join(dataDir, "cache", "viz")
	os.MkdirAll(cacheDir, 0o755)

	resp, err := http.Get(vizBundleURL)
	if err != nil {
		t.Fatalf("download viz bundle: %v", err)
	}
	defer resp.Body.Close()
	tarData, _ := io.ReadAll(resp.Body)

	h := sha256.Sum256([]byte(vizBundleURL))
	cachedPath := filepath.Join(cacheDir, hex.EncodeToString(h[:])+".tar.gz")
	os.WriteFile(cachedPath, tarData, 0644)

	workDir = t.TempDir()
	vizPath = extractVizBundleForTest(t, cachedPath, workDir)
	return vizPath, workDir
}

// ---------- Test: Scenario 1 — Live desktop visualization ----------

func TestVizE2E_LiveDesktopVisualization(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, env.pool, ctx, "viz-live")

	tmpDir := t.TempDir()

	// Create a test viz bundle.
	vizFiles := map[string]string{
		"index.html":   "<html><body><h1>Test Viz</h1><script src='main.js'></script></body></html>",
		"main.js":      "console.log('viz loaded');",
		"style.css":    "body { background: #0a0a0f; }",
		"lettuce-viz.js": "// lettuce-viz SDK stub",
	}
	tarballPath := createTestVizTarGz(t, tmpDir, vizFiles)

	// Serve the tarball over HTTP.
	vizServerURL, vizCleanup := serveVizTarball(t, tarballPath)
	defer vizCleanup()

	vizBundleURL := vizServerURL + "/viz-bundle.tar.gz"
	// Binary URLs in a leaf must pass C2 SSRF validation (https + public FQDN), so
	// the stored config uses a public-looking URL while the test downloads the real
	// bundle from the local (loopback http) server separately.
	vizConfigURL := "https://viz.example.com/viz-bundle.tar.gz"

	// Create a leaf with Binaries["viz"].
	execCfg := defaultExecConfig()
	execCfg.Binaries["viz"] = vizConfigURL
	execCfg.BinaryChecksums["viz"] = "0000000000000000000000000000000000000000000000000000000000000000"

	lf := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name:        "viz-live-test",
		TaskPattern: leaf.PatternParameterSweep,
		ExecConfig:  execCfg,
		ValConfig:   leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:    defaultFTConfig(),
		DataConfig:  defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Verify leaf has viz in binaries.
	if lf.ExecutionConfig.Binaries["viz"] != vizConfigURL {
		t.Fatalf("leaf binaries[viz] = %q, want %q", lf.ExecutionConfig.Binaries["viz"], vizConfigURL)
	}

	// Simulate daemon Prepare() — download, cache, extract viz bundle.
	vizPath, workDir := downloadCacheAndExtract(t, vizBundleURL)

	// Verify index.html exists.
	indexPath := filepath.Join(vizPath, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index.html missing in extracted viz bundle: %v", err)
	}

	// Verify ActiveTaskInfo.viz_bundle_path would be populated.
	if vizPath == "" {
		t.Fatal("viz_bundle_path is empty")
	}

	// Verify viz_read_file can read files from work directory.
	// Write a test output file in the work directory.
	testOutputPath := filepath.Join(workDir, "output.dat")
	os.WriteFile(testOutputPath, []byte("test-output"), 0644)

	data, err := vizReadFile(vizPath, workDir, "output.dat")
	if err != nil {
		t.Fatalf("viz_read_file: %v", err)
	}
	if string(data) != "test-output" {
		t.Fatalf("viz_read_file content = %q, want %q", data, "test-output")
	}

	// Verify viz_read_file rejects path traversal.
	_, err = vizReadFile(vizPath, workDir, "../../../etc/passwd")
	if err == nil {
		t.Fatal("viz_read_file should reject path traversal")
	}
	if !strings.Contains(err.Error(), "traversal") {
		t.Fatalf("expected path traversal error, got: %v", err)
	}

	t.Logf("Scenario 1 PASS: live desktop viz — bundle downloaded, cached, extracted, path traversal blocked")
}

// ---------- Test: Scenario 2 — Dashboard replay ----------

func TestVizE2E_DashboardReplay(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, env.pool, ctx, "viz-replay")

	tmpDir := t.TempDir()
	vizFiles := map[string]string{
		"index.html": "<html><body>Replay Viz</body></html>",
		"main.js":    "// replay viz",
		"style.css":  "body {}",
	}
	tarballPath := createTestVizTarGz(t, tmpDir, vizFiles)
	vizServerURL, vizCleanup := serveVizTarball(t, tarballPath)
	defer vizCleanup()

	vizBundleURL := vizServerURL + "/viz-bundle.tar.gz"
	// See note in TestVizE2E_LiveDesktopVisualization: config uses a public https URL
	// (C2 SSRF validation), download hits the local loopback server.
	vizConfigURL := "https://viz.example.com/viz-bundle.tar.gz"

	// Create leaf with viz bundle.
	execCfg := defaultExecConfig()
	execCfg.Binaries["viz"] = vizConfigURL
	execCfg.BinaryChecksums["viz"] = "0000000000000000000000000000000000000000000000000000000000000000"

	lf := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name:         "viz-replay-test",
		TaskPattern:  leaf.PatternParameterSweep,
		ExecConfig:   execCfg,
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Generate work units.
	genReq := struct {
		ParameterSpace map[string]interface{} `json:"parameter_space"`
	}{
		ParameterSpace: map[string]interface{}{"x": []interface{}{1.0}},
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs/"+lf.ID.String()+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate WUs")
	resp.Body.Close()

	// Register volunteer and submit result with output_data.
	pub := genVolunteerKey(t)
	volID := registerBetaVolunteer(t, env, ctx, pub, "viz-replay-vol", nil)

	outputData := map[string]interface{}{
		"test_key":    "test_value",
		"numeric_val": 42.5,
		"nested": map[string]interface{}{
			"inner": "data",
		},
	}
	outputJSON, _ := json.Marshal(outputData)

	wuID := requestSubmitResult(t, env, ctx, volID, pub, outputJSON)

	// Fetch result via infrastructure API (same path the dashboard results route uses).
	resultsURL := fmt.Sprintf("%s/api/v1/leafs/%s/results?work_unit_id=%s&validation_status=AGREED&limit=1",
		env.httpURL, lf.ID.String(), wuID)
	resp = httpReq(t, "GET", resultsURL, nil)
	requireStatus(t, resp, http.StatusOK, "list results")

	var resultsResp struct {
		Data []struct {
			ID               string                 `json:"id"`
			WorkUnitID       string                 `json:"work_unit_id"`
			OutputData       map[string]interface{}  `json:"output_data"`
			ValidationStatus string                 `json:"validation_status"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &resultsResp)

	if len(resultsResp.Data) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resultsResp.Data))
	}

	result := resultsResp.Data[0]
	if result.ValidationStatus != "AGREED" {
		t.Fatalf("validation_status = %q, want AGREED", result.ValidationStatus)
	}
	if result.OutputData["test_key"] != "test_value" {
		t.Fatalf("output_data.test_key = %v, want test_value", result.OutputData["test_key"])
	}

	// Verify the viz bundle can be downloaded (proxy would do this).
	bundleResp, err := http.Get(vizBundleURL)
	if err != nil {
		t.Fatalf("download viz bundle: %v", err)
	}
	defer bundleResp.Body.Close()
	if bundleResp.StatusCode != 200 {
		t.Fatalf("viz bundle status = %d, want 200", bundleResp.StatusCode)
	}

	t.Logf("Scenario 2 PASS: dashboard replay — result with output_data round-trips, viz bundle downloadable")
}

// ---------- Test: Scenario 3 — No viz fallback ----------

func TestVizE2E_NoVizFallback(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, env.pool, ctx, "viz-noviz")

	// Create a leaf WITHOUT Binaries["viz"].
	execCfg := defaultExecConfig()
	// Ensure no "viz" key exists.
	delete(execCfg.Binaries, "viz")

	lf := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name:         "no-viz-test",
		TaskPattern:  leaf.PatternParameterSweep,
		ExecConfig:   execCfg,
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Verify execution_config.binaries does NOT have "viz".
	if _, hasViz := lf.ExecutionConfig.Binaries["viz"]; hasViz {
		t.Fatal("leaf should NOT have binaries[viz]")
	}

	// Fetch the leaf via GET to verify the API response matches.
	resp := httpReq(t, "GET", env.httpURL+"/api/v1/leafs/"+lf.ID.String(), nil)
	requireStatus(t, resp, http.StatusOK, "get leaf")
	var fetchedLeaf leaf.Leaf
	decodeJSON(t, resp, &fetchedLeaf)

	vizURL, hasViz := fetchedLeaf.ExecutionConfig.Binaries["viz"]
	if hasViz && vizURL != "" {
		t.Fatalf("fetched leaf has binaries[viz] = %q, expected none", vizURL)
	}

	// In the dashboard, this means execution_config?.binaries?.viz is undefined,
	// so the page shows "This leaf does not include visualization".
	// ActiveTaskInfo.viz_bundle_path would be null.

	t.Logf("Scenario 3 PASS: no-viz fallback — leaf without viz bundle correctly has no binaries[viz]")
}

// ---------- Test: Scenario 4 — N-body end-to-end ----------

func TestVizE2E_NbodyEndToEnd(t *testing.T) {
	env, cleanup := setupBetaServer(t)
	defer cleanup()

	ctx := context.Background()
	userID := createTestUser(t, env.pool, ctx, "viz-nbody")

	tmpDir := t.TempDir()

	// Create N-body viz bundle with all expected files.
	nbodyVizFiles := map[string]string{
		"index.html":              "<html><head><script type='importmap'>{\"imports\":{\"three\":\"./lib/three.module.min.js\"}}</script></head><body><canvas id='canvas'></canvas><script type='module' src='main.js'></script></body></html>",
		"main.js":                 "import * as THREE from 'three'; // N-body viz engine\nconsole.log('nbody viz loaded');",
		"style.css":               "#canvas { width: 100%; height: 100%; } body { margin: 0; background: #0a0a0f; }",
		"lettuce-viz.js":          "// lettuce-viz SDK\nexport function createVizClient() { return {}; }",
		"lib/three.module.min.js": "// Three.js stub for testing",
		"lib/addons/controls/OrbitControls.js": "// OrbitControls stub",
	}
	tarballPath := createTestVizTarGz(t, tmpDir, nbodyVizFiles)
	vizServerURL, vizCleanup := serveVizTarball(t, tarballPath)
	defer vizCleanup()

	vizBundleURL := vizServerURL + "/viz-bundle.tar.gz"
	// See note in TestVizE2E_LiveDesktopVisualization: config uses a public https URL
	// (C2 SSRF validation), download hits the local loopback server.
	vizConfigURL := "https://viz.example.com/viz-bundle.tar.gz"

	// Create leaf configured like N-body simulation.
	execCfg := defaultExecConfig()
	execCfg.Binaries["viz"] = vizConfigURL
	execCfg.BinaryChecksums["viz"] = "0000000000000000000000000000000000000000000000000000000000000000"

	lf := createBetaLeaf(t, env, ctx, userID, betaLeafOpts{
		Name:         "nbody-viz-test",
		TaskPattern:  leaf.PatternParameterSweep,
		ExecConfig:   execCfg,
		ValConfig:    leaf.ValidationConfig{RedundancyFactor: 1, AgreementThreshold: 1.0, ComparisonMode: "EXACT", MaxRetries: 3},
		FTConfig:     defaultFTConfig(),
		DataConfig:   defaultDataConfig(),
		CreditConfig: leaf.CreditConfig{CreditPerValidatedWorkUnit: 1.0},
	})

	// Generate work units.
	genReq := struct {
		ParameterSpace map[string]interface{} `json:"parameter_space"`
	}{
		ParameterSpace: map[string]interface{}{"n_particles": []interface{}{100.0}},
	}
	resp := httpReq(t, "POST", env.httpURL+"/api/v1/leafs/"+lf.ID.String()+"/work-units/generate", genReq)
	requireStatus(t, resp, http.StatusAccepted, "generate WUs")
	resp.Body.Close()

	// Register volunteer and submit result with N-body output_data.
	pub := genVolunteerKey(t)
	volID := registerBetaVolunteer(t, env, ctx, pub, "nbody-vol", nil)

	nbodyOutputData := map[string]interface{}{
		"keyframe_snapshots": []map[string]interface{}{
			{
				"time":        0.0,
				"n_particles": 3,
				"positions":   [][]float64{{1.0, 0.0, 0.0}, {0.0, 1.0, 0.0}, {0.0, 0.0, 1.0}},
				"masses":      []float64{1.0, 0.5, 0.3},
			},
			{
				"time":        0.5,
				"n_particles": 3,
				"positions":   [][]float64{{0.9, 0.1, 0.0}, {0.1, 0.9, 0.1}, {0.0, 0.1, 0.9}},
				"masses":      []float64{1.0, 0.5, 0.3},
			},
		},
		"time_series": []map[string]interface{}{
			{
				"time":             0.0,
				"energy_error":     1e-12,
				"core_radius":      0.5,
				"half_mass_radius": 1.2,
				"lagrangian_radii": map[string]float64{
					"r10": 0.3, "r25": 0.6, "r50": 1.2, "r75": 1.8, "r90": 2.5,
				},
			},
			{
				"time":             0.5,
				"energy_error":     2e-12,
				"core_radius":      0.48,
				"half_mass_radius": 1.15,
				"lagrangian_radii": map[string]float64{
					"r10": 0.28, "r25": 0.58, "r50": 1.15, "r75": 1.75, "r90": 2.45,
				},
			},
		},
	}
	outputJSON, _ := json.Marshal(nbodyOutputData)
	wuID := requestSubmitResult(t, env, ctx, volID, pub, outputJSON)

	// Fetch result via infrastructure API.
	resultsURL := fmt.Sprintf("%s/api/v1/leafs/%s/results?work_unit_id=%s&validation_status=AGREED&limit=1",
		env.httpURL, lf.ID.String(), wuID)
	resp = httpReq(t, "GET", resultsURL, nil)
	requireStatus(t, resp, http.StatusOK, "list results")

	var resultsResp struct {
		Data []struct {
			OutputData map[string]interface{} `json:"output_data"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &resultsResp)

	if len(resultsResp.Data) != 1 {
		t.Fatalf("expected 1 result, got %d", len(resultsResp.Data))
	}

	od := resultsResp.Data[0].OutputData

	// Verify keyframe_snapshots round-tripped.
	snapshots, ok := od["keyframe_snapshots"].([]interface{})
	if !ok || len(snapshots) != 2 {
		t.Fatalf("keyframe_snapshots: expected 2 snapshots, got %v", od["keyframe_snapshots"])
	}

	// Verify time_series round-tripped.
	timeSeries, ok := od["time_series"].([]interface{})
	if !ok || len(timeSeries) != 2 {
		t.Fatalf("time_series: expected 2 entries, got %v", od["time_series"])
	}

	// Verify first snapshot structure.
	snap0, ok := snapshots[0].(map[string]interface{})
	if !ok {
		t.Fatalf("snapshot[0] not a map")
	}
	if snap0["n_particles"] != float64(3) {
		t.Fatalf("snapshot[0].n_particles = %v, want 3", snap0["n_particles"])
	}

	// Verify viz bundle serves all expected files.
	expectedFiles := []string{
		"index.html", "main.js", "style.css", "lettuce-viz.js",
		"lib/three.module.min.js", "lib/addons/controls/OrbitControls.js",
	}

	// Extract and verify all files exist.
	vizPath, _ := downloadCacheAndExtract(t, vizBundleURL)

	for _, fileName := range expectedFiles {
		filePath := filepath.Join(vizPath, filepath.FromSlash(fileName))
		if _, err := os.Stat(filePath); err != nil {
			t.Errorf("expected file %q missing in viz bundle: %v", fileName, err)
		}
	}

	// Verify extracted index.html content.
	indexContent, err := os.ReadFile(filepath.Join(vizPath, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(indexContent), "importmap") {
		t.Fatal("index.html should contain importmap for Three.js")
	}

	t.Logf("Scenario 4 PASS: N-body e2e — output_data with snapshots+timeseries round-trips, all %d viz bundle files present", len(expectedFiles))
}
