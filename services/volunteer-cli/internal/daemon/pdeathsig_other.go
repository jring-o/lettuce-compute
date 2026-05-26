//go:build !linux && !windows

package daemon

import "os/exec"

// setPdeathsig is a no-op on non-Linux Unix (macOS, FreeBSD).
// Pdeathsig is Linux-specific. On these platforms, we rely on
// process group cleanup (SIGKILL to pgid) in Terminate().
func setPdeathsig(cmd *exec.Cmd) {}
