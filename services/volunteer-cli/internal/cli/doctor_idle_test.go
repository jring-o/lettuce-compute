package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// doctor's "idle time" row: whether "run when idle" can read this computer's
// idle time. Without it, a volunteer whose machine cannot report idle time
// read "no blocking failures" and never computed.
func TestCheckIdleDetection(t *testing.T) {
	readable := func() (int, error) { return 240, nil }
	unreadable := func() (int, error) { return 0, errors.New("no idle source answered") }

	cases := []struct {
		name                       string
		daemonFailing, daemonKnown bool
		probe                      func() (int, error)
		wantTag                    string
		wantFails, wantWarns       int
		wantText                   []string
	}{
		{"the running daemon cannot read it", true, true, readable, "fail", 1, 0,
			[]string{"running daemon cannot read", "never starts work", "schedule clear"}},
		{"no daemon, unreadable here", false, false, unreadable, "warn", 0, 1,
			[]string{"cannot be read from this terminal", "no idle source answered", "would never start work", "schedule clear"}},
		{"daemon reads it, this terminal cannot", false, true, unreadable, "info", 0, 0,
			[]string{"cannot be read from this terminal", "reports no trouble"}},
		{"readable", false, false, readable, "ok  ", 0, 0,
			[]string{"readable", "4m", "after 5 min idle"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, rep := doctorRows(func(rep *doctorReport) {
				checkIdleDetection(rep, 5, tc.daemonFailing, tc.daemonKnown, tc.probe)
			})
			if !strings.Contains(out, tc.wantTag+"  idle time") {
				t.Errorf("row = %q, want tag %q on an \"idle time\" row", out, tc.wantTag)
			}
			if rep.fails != tc.wantFails || rep.warns != tc.wantWarns {
				t.Errorf("fails, warns = %d, %d; want %d, %d", rep.fails, rep.warns, tc.wantFails, tc.wantWarns)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(out, want) {
					t.Errorf("row = %q, want it to contain %q", out, want)
				}
			}
		})
	}
}

// fakeDaemonNotices serves GET /api/v1/notices the way a running daemon does
// and writes the daemon.json that points doctor at it.
func fakeDaemonNotices(t *testing.T, notices []daemon.Notice) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/notices" || r.Header.Get("Authorization") != "Bearer test-token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"notices": notices, "latest_id": len(notices)})
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	dir := t.TempDir()
	info, _ := json.Marshal(map[string]any{"port": port, "token": "test-token", "pid": os.Getpid()})
	if err := os.WriteFile(filepath.Join(dir, "daemon.json"), info, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDaemonIdleUnavailable(t *testing.T) {
	if failing, known := daemonIdleUnavailable(t.TempDir()); failing || known {
		t.Errorf("no daemon: failing, known = %v, %v; want false, false", failing, known)
	}

	live := daemon.Notice{ID: 1, Level: daemon.NoticeWarn, Code: daemon.IdleUnavailableNoticeCode, Message: "m", Count: 1}
	if failing, known := daemonIdleUnavailable(fakeDaemonNotices(t, []daemon.Notice{live})); !failing || !known {
		t.Errorf("a live idle notice: failing, known = %v, %v; want true, true", failing, known)
	}

	resolved := live
	at := time.Now()
	resolved.ResolvedAt = &at
	other := daemon.Notice{ID: 2, Level: daemon.NoticeWarn, Code: "no_work", Message: "m", Count: 1}
	if failing, known := daemonIdleUnavailable(fakeDaemonNotices(t, []daemon.Notice{resolved, other})); failing || !known {
		t.Errorf("only a resolved idle notice: failing, known = %v, %v; want false, true", failing, known)
	}
}
