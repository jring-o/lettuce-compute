package daemon

import "os/exec"

// ProcessGroup manages child processes so they are killed when the daemon exits.
// On Windows, this uses a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
// On Unix, this uses setpgid and tracks process group IDs for explicit cleanup.
type ProcessGroup interface {
	// ConfigureCommand sets OS-specific attributes on cmd before Start().
	// On Unix, this sets Setpgid=true (and Pdeathsig on Linux).
	// On Windows, this is a no-op (processes are added after start).
	ConfigureCommand(cmd *exec.Cmd)

	// Add assigns a running process to the group by PID.
	// On Windows, this calls AssignProcessToJobObject.
	// On Unix, this records the PGID for cleanup.
	Add(pid int) error

	// Terminate kills all processes in the group.
	Terminate()

	// Close releases OS resources (e.g., the Job Object handle).
	Close()

	// ReleaseChildren removes the kill-on-close behavior so child processes
	// survive when the daemon exits. Used for "suspend and quit" — frozen
	// orphan processes stay alive for the next daemon launch to resume.
	ReleaseChildren()

	// CPUSeconds is the cumulative CPU time of the processes in the group,
	// keyed by group (a process group id on Unix, the one Job Object on
	// Windows), so the yield monitor can subtract Lettuce's native tasks
	// from the machine's load (TB-83). A group whose processes have all
	// exited is absent; the caller keeps what it had used.
	CPUSeconds() (map[string]float64, error)
}
