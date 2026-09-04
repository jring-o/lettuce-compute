package cli

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// tb62MockHead is the smallest head that proves a dial arrived: it answers
// the status probe the connect path uses, counts registrations and refuses
// each one with a non-retriable error, so `start` gives up on the head and
// returns instead of running the daemon.
type tb62MockHead struct {
	lettucev1.UnimplementedVolunteerServiceServer
	mu            sync.Mutex
	registerCalls int
}

func (m *tb62MockHead) GetServerStatus(context.Context, *lettucev1.GetServerStatusRequest) (*lettucev1.GetServerStatusResponse, error) {
	return &lettucev1.GetServerStatusResponse{Version: "tb62-mock"}, nil
}

func (m *tb62MockHead) RegisterVolunteer(context.Context, *lettucev1.RegisterVolunteerRequest) (*lettucev1.RegisterVolunteerResponse, error) {
	m.mu.Lock()
	m.registerCalls++
	m.mu.Unlock()
	return nil, status.Error(codes.Internal, "tb62 mock head refuses registration so start exits")
}

func (m *tb62MockHead) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registerCalls
}

// TestTB62_StartDialsAHeadStoredAsURL is the end-to-end half of TB-62, the
// filing's own repro: `init` a profile, rewrite its head entry to the shape
// desktop-v2.0.0 wrote (the typed URL as the gRPC address and as the name, no
// HTTP address), and `start`. The daemon must dial the head, not fail "name
// resolver error: produced zero addresses" three times and skip it. The mock
// head is plain-text, so the stored URL is http://; the repair path is the
// same one an https:// entry takes, plus the insecure flag.
func TestTB62_StartDialsAHeadStoredAsURL(t *testing.T) {
	withContainerBackend(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	head := &tb62MockHead{}
	srv := grpc.NewServer()
	lettucev1.RegisterVolunteerServiceServer(srv, head)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.yaml")
	initCmd := newRootCmd()
	initCmd.SetArgs([]string{"init", "--config", cfgFile, "--data-dir", dir,
		"--cpu-cores", "1", "--memory-mb", "1024", "--server", lis.Addr().String()})
	initCmd.SetOut(io.Discard)
	initCmd.SetErr(io.Discard)
	captureStdout(t, func() {
		if err := initCmd.Execute(); err != nil {
			t.Fatalf("init failed: %v", err)
		}
	})

	// Rewrite the entry init wrote into the 2.0.0 shape, in place.
	storedURL := "http://" + lis.Addr().String()
	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- grpc_address:"):
			line = strings.SplitN(line, "grpc_address:", 2)[0] + "grpc_address: " + storedURL
		case strings.HasPrefix(trimmed, "http_address:"):
			continue
		case strings.HasPrefix(trimmed, "name:"):
			line = strings.SplitN(line, "name:", 2)[0] + "name: " + storedURL
		}
		lines = append(lines, line)
	}
	rewritten := strings.Join(lines, "\n")
	if !strings.Contains(rewritten, "grpc_address: "+storedURL) || strings.Contains(rewritten, "http_address:") {
		t.Fatalf("rewrite to the 2.0.0 shape failed:\n%s", rewritten)
	}
	if err := os.WriteFile(cfgFile, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"start", "--config", cfgFile, "--data-dir", dir, "--log-level", "debug"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err = captureStderrErr(t, func() error { return cmd.Execute() })
	if err == nil {
		t.Fatal("start returned nil; the mock head refuses every registration, so it must have given up")
	}
	if !strings.Contains(err.Error(), "could not connect to any configured server") {
		t.Fatalf("start failed for another reason: %v", err)
	}

	logBytes, err := os.ReadFile(filepath.Join(dir, "logs", "volunteer.log"))
	if err != nil {
		t.Fatalf("reading the daemon log: %v", err)
	}
	log := string(logBytes)
	if got := head.calls(); got == 0 {
		t.Errorf("the head was never dialled: RegisterVolunteer calls = 0 (the stored URL %q was used as the gRPC target unrepaired):\n%s", storedURL, log)
	}
	if strings.Contains(log, "name resolver error") {
		t.Errorf("the daemon log still shows the resolver failure the repair exists to prevent:\n%s", log)
	}
	if !strings.Contains(log, "repaired stored head address") {
		t.Errorf("the daemon log does not say the stored address was repaired:\n%s", log)
	}
	if !strings.Contains(log, storedURL) {
		t.Errorf("the repair line does not name the old address %s:\n%s", storedURL, log)
	}
}

// captureStderrErr runs fn with stderr redirected to a pipe (the daemon logger
// writes to stderr as well as its file) and returns fn's error.
func captureStderrErr(t *testing.T, fn func() error) error {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(done)
	}()
	defer func() {
		w.Close()
		<-done
		os.Stderr = orig
	}()
	return fn()
}
