package daemon

// ProcessHandle allows suspending and resuming a running work unit process,
// and giving it a new share of the CPU budget while it runs.
// Implementations are platform-specific (see suspend_unix.go, suspend_windows.go).
type ProcessHandle interface {
	Suspend() error
	Resume() error
	PID() int
	// SetCPUShare gives the running task shareCores of CPU — its equal share
	// of the volunteer's budget, recomputed whenever a task starts or
	// finishes (TB-75). Containers have their quota rewritten by the engine;
	// native processes go through the limiter (cgroup cpu.max, the Job
	// Object's rate). Best-effort: a failure is logged by the caller and the
	// task keeps the cap it had.
	SetCPUShare(shareCores float64) error
}
