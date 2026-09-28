package daemon

// ProcessHandle allows suspending and resuming a running work unit process.
// Implementations are platform-specific (see suspend_unix.go, suspend_windows.go).
type ProcessHandle interface {
	Suspend() error
	Resume() error
	PID() int
}
