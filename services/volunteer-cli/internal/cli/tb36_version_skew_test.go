package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TB-36: the boot-time version comparison fired at WARN on every volunteer at every
// boot whenever a client-only release put the fleet ahead of its heads — the normal
// state after most releases — and its text asserted a matching-builds requirement
// that does not exist. A tester filtering on WARN saw it on all four hosts with
// nothing wrong. The note is now INFO with honest wording; the head's own "too old —
// run update" rejection remains the real, loud compatibility gate.

// skewRecords runs logVersionSkew and returns the emitted records as (level, line).
func skewRecords(t *testing.T, head, vol string) (count int, level slog.Level, line string) {
	t.Helper()
	var buf bytes.Buffer
	var lv slog.Level
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logVersionSkew(logger, "test-head", head, vol)
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return 0, lv, ""
	}
	lines := strings.Split(out, "\n")
	first := lines[0]
	switch {
	case strings.Contains(first, "level=WARN"):
		lv = slog.LevelWarn
	case strings.Contains(first, "level=INFO"):
		lv = slog.LevelInfo
	case strings.Contains(first, "level=DEBUG"):
		lv = slog.LevelDebug
	}
	return len(lines), lv, first
}

// TestTB36_VersionSkewLogsInfoNotWarn is the regression: a newer volunteer against an
// older head (v0.10.7 client, v0.10.6 head — the live false-fire) must produce an
// Info line, not a WARN, and the line must not claim builds have to match.
func TestTB36_VersionSkewLogsInfoNotWarn(t *testing.T) {
	n, level, line := skewRecords(t, "v0.10.6", "0.10.7")
	if n != 1 {
		t.Fatalf("expected exactly one log line for a version skew, got %d", n)
	}
	if level == slog.LevelWarn {
		t.Errorf("version skew logged at WARN — it cries wolf on every client-only release (TB-36); want INFO")
	}
	if level != slog.LevelInfo {
		t.Errorf("version skew logged at %v, want INFO", level)
	}
	if strings.Contains(line, "must run matching builds") {
		t.Errorf("log line still asserts a matching-builds requirement that does not exist: %s", line)
	}
}

// TestTB36_NoNoteWhenMatchedEmptyOrDev pins the silent cases: matched versions
// (including the v-prefix normalization), unknown versions, and local dev builds
// produce nothing at all.
func TestTB36_NoNoteWhenMatchedEmptyOrDev(t *testing.T) {
	cases := []struct{ head, vol, name string }{
		{"v0.10.7", "0.10.7", "matched modulo v-prefix"},
		{"", "0.10.7", "unknown head version"},
		{"v0.10.6", "", "unknown volunteer version"},
		{"dev", "0.10.7", "dev head"},
		{"v0.10.6", "dev", "dev volunteer"},
	}
	for _, c := range cases {
		if n, _, line := skewRecords(t, c.head, c.vol); n != 0 {
			t.Errorf("%s: expected silence, got %q", c.name, line)
		}
	}
}
