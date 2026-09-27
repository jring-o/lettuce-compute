package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// A container result must report the CPU time and peak memory the container
// actually used. The engine answers stats only while a container runs (after
// it exits Docker returns an empty reading and Podman an error body), and its
// inspect call carries only the configured memory LIMIT — so a runtime that
// reads usage after the wait, from inspect, reports 0 CPU seconds, 1 core and
// the limit as the peak on every container unit.
//
// These tests drive the production engine client over HTTP against a fake
// engine that behaves the way Docker and Podman do, so they exercise the real
// request and decoding path rather than a mocked interface: the mocked client
// is exactly how the missing measurement went unnoticed.

const (
	fakeEngineImage   = "registry.example/leaf@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fakeEngineLimitMB = 7000
	fakeEngineWorkMB  = 300 // the container's working set: well under its limit
	fakeEngineCacheMB = 200 // inactive page cache the engine counts in "usage"
	fakeEngineCores   = 3   // CPU the fake container keeps busy
)

// fakeEngine is a Docker Engine API server holding one container that runs
// from startedAt until exitAt, keeping fakeEngineCores busy the whole time.
type fakeEngine struct {
	t      *testing.T
	podman bool // answer stats like Podman's compatibility API (else like Docker)

	mu        sync.Mutex
	id        string
	startedAt time.Time
	exitAt    time.Time
	runFor    time.Duration // how long the container runs once started
	memLimit  int64
	firstUser uint64 // user-mode CPU in the first stats reading served while running
	lastUser  uint64 // ... and in the last one
	lastSys   uint64
	served    int // stats readings served while the container ran
}

var fakeEngineVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func newFakeEngine(t *testing.T, podman bool, runFor time.Duration) (*fakeEngine, *httptest.Server) {
	t.Helper()
	fe := &fakeEngine{t: t, podman: podman, runFor: runFor}
	srv := httptest.NewServer(http.HandlerFunc(fe.serve))
	t.Cleanup(srv.Close)
	return fe, srv
}

// adopt makes the engine hold a container a previous session started long
// ago, which is still running and will exit runFor from now.
func (fe *fakeEngine) adopt(id string, runningFor time.Duration) {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	now := time.Now()
	fe.id = id
	fe.startedAt = now.Add(-runningFor)
	fe.exitAt = now.Add(fe.runFor)
	fe.memLimit = fakeEngineLimitMB * 1024 * 1024
}

// cpuAt is the container's cumulative (user, kernel) CPU nanoseconds at t:
// fakeEngineCores busy from start to exit, 5 % of it in the kernel.
func (fe *fakeEngine) cpuAt(t time.Time) (user, kernel uint64) {
	if t.After(fe.exitAt) {
		t = fe.exitAt
	}
	total := uint64(t.Sub(fe.startedAt).Nanoseconds()) * fakeEngineCores
	kernel = total / 20
	return total - kernel, kernel
}

func (fe *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	path := fakeEngineVersionPrefix.ReplaceAllString(r.URL.Path, "")
	writeJSON := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", "1.41")
		w.Header().Set("Ostype", "linux")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))

	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		writeJSON(http.StatusOK, map[string]any{"Id": "sha256:1111", "Config": map[string]any{}})

	case path == "/containers/json" && r.Method == http.MethodGet:
		writeJSON(http.StatusOK, []any{})

	case path == "/containers/create":
		var body struct {
			HostConfig struct {
				Memory int64 `json:"Memory"`
			} `json:"HostConfig"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fe.mu.Lock()
		fe.id = "fresh-container"
		fe.memLimit = body.HostConfig.Memory
		fe.mu.Unlock()
		writeJSON(http.StatusCreated, map[string]any{"Id": "fresh-container", "Warnings": []string{}})

	case strings.HasSuffix(path, "/start"):
		fe.mu.Lock()
		fe.startedAt = time.Now()
		fe.exitAt = fe.startedAt.Add(fe.runFor)
		fe.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case strings.HasSuffix(path, "/wait"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fe.mu.Lock()
		exitAt := fe.exitAt
		fe.mu.Unlock()
		select {
		case <-time.After(time.Until(exitAt)):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": 0})

	case strings.HasSuffix(path, "/logs"):
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(path, "/stats"):
		fe.serveStats(w, writeJSON)

	case strings.HasSuffix(path, "/json") && strings.HasPrefix(path, "/containers/"):
		// Inspect: the engine reports the configured limit, never the usage.
		fe.mu.Lock()
		limit, id := fe.memLimit, fe.id
		fe.mu.Unlock()
		writeJSON(http.StatusOK, map[string]any{
			"Id":         id,
			"State":      map[string]any{"Status": "exited", "Running": false, "ExitCode": 0},
			"HostConfig": map[string]any{"Memory": limit},
		})

	case r.Method == http.MethodDelete, strings.HasSuffix(path, "/stop"), strings.HasSuffix(path, "/unpause"):
		w.WriteHeader(http.StatusNoContent)

	default:
		fe.t.Errorf("fake engine: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveStats answers a one-shot stats request the way the real engines do:
// a full reading while the container runs; once it has exited, Docker's empty
// reading (no timestamp, all zeros) or Podman's error body, both with 200.
func (fe *fakeEngine) serveStats(w http.ResponseWriter, writeJSON func(int, any)) {
	fe.mu.Lock()
	defer fe.mu.Unlock()
	now := time.Now()
	if !now.Before(fe.exitAt) {
		if fe.podman {
			writeJSON(http.StatusOK, map[string]any{"cause": "container is stopped", "message": "container is stopped", "response": 500})
		} else {
			writeJSON(http.StatusOK, map[string]any{"name": "/fake", "id": fe.id})
		}
		return
	}
	user, kernel := fe.cpuAt(now)
	if fe.served == 0 {
		fe.firstUser = user
	}
	fe.lastUser, fe.lastSys = user, kernel
	fe.served++

	mem := map[string]any{"limit": fe.memLimit}
	if fe.podman {
		// Podman's compatibility API sends usage with no memory.stat
		// breakdown (measured on Podman 5.8), which is taken as reported.
		mem["usage"] = fakeEngineWorkMB * 1024 * 1024
	} else {
		// Docker on cgroup v2 reports memory.current, page cache included,
		// with the breakdown that lets a reader take the cache back out.
		mem["usage"] = (fakeEngineWorkMB + fakeEngineCacheMB) * 1024 * 1024
		mem["stats"] = map[string]any{"inactive_file": fakeEngineCacheMB * 1024 * 1024}
	}
	writeJSON(http.StatusOK, map[string]any{
		"read": now.Format(time.RFC3339Nano),
		"id":   fe.id,
		"cpu_stats": map[string]any{
			"cpu_usage": map[string]any{
				"total_usage":         user + kernel,
				"usage_in_usermode":   user,
				"usage_in_kernelmode": kernel,
			},
			"online_cpus": 8,
		},
		"memory_stats": mem,
	})
}

// newFakeEngineRuntime is the production container runtime over the
// production engine client, pointed at the fake engine.
func newFakeEngineRuntime(t *testing.T, srv *httptest.Server) *ContainerRuntime {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dc, err := NewDockerClientWrapperWithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://"), logger)
	if err != nil {
		t.Fatalf("engine client: %v", err)
	}
	t.Cleanup(func() { dc.Close() })
	cr := NewContainerRuntimeWithClient(t.TempDir(), logger, dc)
	cr.SetMemoryCeilingMB(8000)
	return cr
}

func fakeEngineWorkUnit(id string) *WorkUnit {
	return &WorkUnit{
		ID:              id,
		LeafID:          "leaf-1",
		DeadlineSeconds: 120,
		ExecutionSpec:   ExecutionSpec{Image: fakeEngineImage, MaxMemoryMB: fakeEngineLimitMB},
	}
}

func runOnFakeEngine(t *testing.T, cr *ContainerRuntime, wu *WorkUnit, orphanID string) *ExecutionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	prep, err := cr.Prepare(ctx, wu)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cr.Cleanup(prep)
	if err := os.WriteFile(filepath.Join(prep.WorkDir, "output", "output.dat"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	prep.OrphanContainerID = orphanID
	result, err := cr.Execute(ctx, wu, prep)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

// checkMeasuredCPU asserts the result reports exactly the CPU the engine
// reported between the first counter the runtime could count from (want0,
// user-mode nanoseconds) and the last reading taken while the container ran.
func checkMeasuredCPU(t *testing.T, fe *fakeEngine, m ExecutionMetrics, want0 uint64) {
	t.Helper()
	fe.mu.Lock()
	served, lastUser := fe.served, fe.lastUser
	fe.mu.Unlock()
	if served < 2 {
		t.Errorf("the runtime read the running container's stats %d times; a %s run must be sampled while it runs", served, fe.runFor)
	}
	if m.CPUSecondsUser <= 0 {
		t.Errorf("CPUSecondsUser = %v: the container's CPU time was not measured", m.CPUSecondsUser)
	} else if served > 0 {
		wantUser := float64(lastUser-want0) / 1e9
		if diff := m.CPUSecondsUser - wantUser; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("CPUSecondsUser = %.6f, want %.6f (the engine's last reading while the container ran)", m.CPUSecondsUser, wantUser)
		}
	}
	if m.CPUSecondsSystem <= 0 {
		t.Errorf("CPUSecondsSystem = %v, want the engine's kernel-mode time", m.CPUSecondsSystem)
	}
	total := m.CPUSecondsUser + m.CPUSecondsSystem
	if ceiling := float64(fakeEngineCores) * fe.runFor.Seconds() * 1.05; total > ceiling {
		t.Errorf("CPU seconds = %.1f, more than %d cores for the whole %s run (%.1f): CPU from before this run was counted", total, fakeEngineCores, fe.runFor, ceiling)
	}
	if m.CPUCoresUsed < 2 || m.CPUCoresUsed > fakeEngineCores {
		t.Errorf("CPUCoresUsed = %d, want 2..%d for a container keeping %d cores busy", m.CPUCoresUsed, fakeEngineCores, fakeEngineCores)
	}
	if m.PeakMemoryMB != fakeEngineWorkMB {
		t.Errorf("PeakMemoryMB = %d, want %d (the working set, not the %d MB limit or the page cache)", m.PeakMemoryMB, fakeEngineWorkMB, fakeEngineLimitMB)
	}
}

func TestContainerResultReportsMeasuredUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		podman bool
	}{
		{"docker", false},
		{"podman", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fe, srv := newFakeEngine(t, tc.podman, 4500*time.Millisecond)
			cr := newFakeEngineRuntime(t, srv)
			result := runOnFakeEngine(t, cr, fakeEngineWorkUnit("5f0c7c0e-4a55-4d59-9b2e-2f6a1d0c9a01"), "")
			// A container created for this run: its counters start at zero.
			checkMeasuredCPU(t, fe, result.Metrics, 0)
		})
	}
}

// A container adopted from a previous session (paused at quit, or left
// running by a crash) has counters that include the hours it ran before this
// session. The result's wall clock covers only this session, so its CPU time
// must too — otherwise a unit adopted near its end reports many times the
// cores it could ever use.
func TestAdoptedContainerResultCountsOnlyThisSessionsCPU(t *testing.T) {
	t.Parallel()
	fe, srv := newFakeEngine(t, true, 4500*time.Millisecond)
	fe.adopt("orphan-container", 5*time.Hour)
	cr := newFakeEngineRuntime(t, srv)
	result := runOnFakeEngine(t, cr, fakeEngineWorkUnit("5f0c7c0e-4a55-4d59-9b2e-2f6a1d0c9a02"), "orphan-container")
	fe.mu.Lock()
	baseline := fe.firstUser
	fe.mu.Unlock()
	checkMeasuredCPU(t, fe, result.Metrics, baseline)
}
