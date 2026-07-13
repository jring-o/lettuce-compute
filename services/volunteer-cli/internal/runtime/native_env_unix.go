//go:build !windows

package runtime

import "strings"

// nativeEnvAllowed reports whether a host environment variable may be inherited by
// an opted-in native leaf on Unix. Everything else in os.Environ() is dropped so the
// child never sees the volunteer's secrets — AWS_*, GITHUB_TOKEN, cloud credentials
// (BG-12). PATH/HOME/TMPDIR are load-bearing for an ordinary process; LANG and LC_*
// set locale (design finding #7).
func nativeEnvAllowed(key string) bool {
	switch key {
	case "PATH", "HOME", "TMPDIR", "LANG":
		return true
	}
	return strings.HasPrefix(key, "LC_")
}
