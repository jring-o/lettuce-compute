//go:build linux

package resource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeIdleTools puts the given shell scripts, and nothing else, on PATH, so
// GetIdleSeconds runs exactly these as dbus-send and xprintidle.
func fakeIdleTools(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// A machine with neither dbus-send nor xprintidle — a headless server, a
// container — used to read as "0 seconds idle, no error", so "run when idle"
// waited forever and nothing said why.
func TestGetIdleSeconds_NoSourceIsAnError(t *testing.T) {
	fakeIdleTools(t, nil)
	secs, err := GetIdleSeconds()
	if err == nil {
		t.Fatalf("GetIdleSeconds() with no idle source = (%d, nil), want an error: a failure must not look like a busy machine", secs)
	}
	for _, want := range []string{"idle time cannot be read", "D-Bus ScreenSaver", "xprintidle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Both tools present but nothing to talk to — no session bus, no display,
// as over SSH or under a service manager — is the same failure, and the
// error carries what each tool printed.
func TestGetIdleSeconds_ToolsWithoutASessionAreAnError(t *testing.T) {
	fakeIdleTools(t, map[string]string{
		"dbus-send":  "echo 'Failed to open connection to \"session\" message bus: Unable to autolaunch a dbus-daemon without a $DISPLAY for X11' >&2; exit 1",
		"xprintidle": "echo \"couldn't open display\" >&2; exit 1",
	})
	secs, err := GetIdleSeconds()
	if err == nil {
		t.Fatalf("GetIdleSeconds() with failing tools = (%d, nil), want an error", secs)
	}
	for _, want := range []string{"Unable to autolaunch a dbus-daemon", "couldn't open display"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry the tool's own message %q", err, want)
		}
	}
}

func TestGetIdleSeconds_XprintidleAnswers(t *testing.T) {
	fakeIdleTools(t, map[string]string{
		"dbus-send":  "echo 'Error org.freedesktop.DBus.Error.ServiceUnknown' >&2; exit 1",
		"xprintidle": "echo 600000",
	})
	if secs, err := GetIdleSeconds(); err != nil || secs != 600 {
		t.Errorf("GetIdleSeconds() = (%d, %v), want (600, nil)", secs, err)
	}
}

func TestGetIdleSeconds_DBusAnswers(t *testing.T) {
	fakeIdleTools(t, map[string]string{
		"dbus-send": "printf 'method return time=1 sender=:1.2 -> destination=:1.3 serial=4 reply_serial=2\\n   uint32 125000\\n'",
	})
	if secs, err := GetIdleSeconds(); err != nil || secs != 125 {
		t.Errorf("GetIdleSeconds() = (%d, %v), want (125, nil)", secs, err)
	}
}
