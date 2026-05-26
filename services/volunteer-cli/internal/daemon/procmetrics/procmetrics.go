package procmetrics

// ProcessMetrics holds per-process resource usage metrics.
type ProcessMetrics struct {
	MemoryRSSMB    *float64
	VirtualMemoryMB *float64
	CPUUsagePct    *float64
	DiskReadMB     *float64
	DiskWrittenMB  *float64
}

// Reader reads process metrics for a given PID.
type Reader interface {
	Read(pid int) (*ProcessMetrics, error)
}

// ContainerReader reads metrics for a container by ID.
type ContainerReader interface {
	ReadContainer(containerID string) (*ProcessMetrics, error)
}

// NewReader returns a platform-specific Reader implementation.
func NewReader() Reader {
	return newPlatformReader()
}
