//go:build linux

package resource

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// LinuxLimiter enforces resource limits using cgroups v2 (preferred) or
// prlimit/sched_setaffinity as fallback.
type LinuxLimiter struct {
	logger     *slog.Logger
	useCgroups bool

	// pins records the CPUs each running task was pinned to on the affinity
	// fallback, so the next task is pinned to the CPUs the fewest tasks are
	// on and the tasks' grants spread across the budget's CPUs.
	pinMu sync.Mutex
	pins  map[int][]int
}

func newPlatformLimiter(logger *slog.Logger) Limiter {
	return NewLinuxLimiter(logger)
}

// NewLinuxLimiter creates a limiter, detecting cgroups v2 availability.
func NewLinuxLimiter(logger *slog.Logger) *LinuxLimiter {
	useCgroups := detectCgroupsV2()
	if useCgroups {
		logger.Info("using cgroups v2 for resource limits")
	} else {
		logger.Info("cgroups v2 not available, falling back to prlimit/affinity")
	}
	return &LinuxLimiter{
		logger:     logger,
		useCgroups: useCgroups,
	}
}

// detectCgroupsV2 checks if cgroups v2 is available and writable.
func detectCgroupsV2() bool {
	info, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return false
	}
	// Check if we can create a subdirectory (need cgroup delegation).
	testDir := "/sys/fs/cgroup/lettuce-probe"
	if err := os.Mkdir(testDir, 0o755); err != nil {
		return false
	}
	os.Remove(testDir)
	return info != nil
}

// Apply configures the exec.Cmd before process start.
func (l *LinuxLimiter) Apply(cmd *exec.Cmd, limits *TaskLimits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Create new process group for clean signal delivery.
	cmd.SysProcAttr.Setpgid = true
	return nil
}

// Enforce applies post-start resource limits (cgroups or prlimit+affinity),
// and on either path runs the task at the lowest priority.
func (l *LinuxLimiter) Enforce(pid int, limits *TaskLimits) (func(), error) {
	l.lowerPriority(pid)
	if l.useCgroups {
		return l.enforceCgroups(pid, limits)
	}
	return l.enforceFallback(pid, limits)
}

// lowestNice is the nice value every task runs at: the lowest priority the
// ordinary scheduler offers, so the owner's own programs are served first
// whenever the CPUs are contended. It lowers only the task's priority, never
// what it may use: a task on an idle machine runs as fast as before.
const lowestNice = 19

// lowerPriority sets every thread of pid to lowestNice. On Linux a nice value
// belongs to a thread, not to a process, and a task's runtime may already
// have started threads by the time Enforce runs; threads and child processes
// created afterwards inherit the value. Best-effort: a failure is logged and
// the task runs on at the priority it had.
func (l *LinuxLimiter) lowerPriority(pid int) {
	var failed int
	var lastErr error
	tids := threadIDs(pid)
	for _, tid := range tids {
		// A thread that exited since the listing (ESRCH) needs nothing.
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, lowestNice); err != nil && err != syscall.ESRCH {
			failed++
			lastErr = err
		}
	}
	if failed > 0 {
		l.logger.Warn("could not lower the task's priority; it runs at normal priority",
			"pid", pid, "threads", len(tids), "failed", failed, "error", lastErr)
		return
	}
	l.logger.Debug("set task priority", "pid", pid, "nice", lowestNice, "threads", len(tids))
}

// threadIDs lists the threads of pid from /proc, or pid alone when the listing
// cannot be read.
func threadIDs(pid int) []int {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return []int{pid}
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	if len(tids) == 0 {
		return []int{pid}
	}
	return tids
}

// cgroupPathFor is the per-process cgroup v2 scope the cgroup path creates.
func cgroupPathFor(pid int) string {
	return fmt.Sprintf("/sys/fs/cgroup/lettuce-%d", pid)
}

// writeCPUMax sets a cgroup's CPU bandwidth to cores: cpu.max =
// "{quota} {period}" with quota = cores × period, so 3 cores is
// "300000 100000". A grant of 0 lifts the cap ("max 100000").
func writeCPUMax(cgroupPath string, cores int) error {
	quota, period := runtime.CFSQuota(cores)
	cpuMax := fmt.Sprintf("max %d", runtime.CFSPeriodMicros)
	if quota > 0 {
		cpuMax = fmt.Sprintf("%d %d", quota, period)
	}
	if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.max"), []byte(cpuMax), 0o644); err != nil {
		return fmt.Errorf("set cpu.max: %w", err)
	}
	return nil
}

// writeCPUWeight gives a cgroup the lowest CPU weight, cpu.weight 1 (the
// default is 100): when the CPUs are contended, every thread and child in the
// group yields to the owner's programs. Its cpu.max still caps what it may use.
func writeCPUWeight(cgroupPath string) error {
	if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.weight"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("set cpu.weight: %w", err)
	}
	return nil
}

// enforceCgroups creates a cgroup v2 scope for the process with memory and CPU limits.
func (l *LinuxLimiter) enforceCgroups(pid int, limits *TaskLimits) (func(), error) {
	cgroupPath := cgroupPathFor(pid)

	if err := os.MkdirAll(cgroupPath, 0o755); err != nil {
		return nil, fmt.Errorf("create cgroup: %w", err)
	}

	cleanup := func() {
		os.Remove(cgroupPath) // rmdir — only works if empty (after process exits)
	}

	// Set memory limit.
	if limits.MaxMemoryMB > 0 {
		memBytes := int64(limits.MaxMemoryMB) * 1024 * 1024
		memPath := filepath.Join(cgroupPath, "memory.max")
		if err := os.WriteFile(memPath, []byte(strconv.FormatInt(memBytes, 10)), 0o644); err != nil {
			cleanup()
			return nil, fmt.Errorf("set memory.max: %w", err)
		}
		l.logger.Debug("cgroup memory limit set", "bytes", memBytes)
	}

	// Set the CPU limit to this task's grant — not, as before, the whole
	// budget for every process, which let N tasks use N times the limit
	// (TB-75).
	if limits.CPU.Cores > 0 {
		if err := writeCPUMax(cgroupPath, limits.CPU.Cores); err != nil {
			cleanup()
			return nil, err
		}
		quota, period := runtime.CFSQuota(limits.CPU.Cores)
		l.logger.Debug("cgroup CPU limit set", "quota", quota, "period", period, "cores", limits.CPU.Cores, "budget_cores", limits.CPU.BudgetCores)
	}

	// Lowest weight: covers the whole group, including threads and children
	// the per-thread nice value (lowerPriority) could miss. A courtesy, so a
	// failure is logged and the task keeps its limits.
	if err := writeCPUWeight(cgroupPath); err != nil {
		l.logger.Warn("could not give the task's cgroup the lowest CPU weight", "pid", pid, "cgroup", cgroupPath, "error", err)
	}

	// Assign the process to the cgroup.
	procsPath := filepath.Join(cgroupPath, "cgroup.procs")
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		cleanup()
		return nil, fmt.Errorf("assign process to cgroup: %w", err)
	}

	l.logger.Info("process assigned to cgroup", "pid", pid, "cgroup", cgroupPath)
	return cleanup, nil
}

// rlimit64 matches the kernel's struct rlimit64.
type rlimit64 struct {
	Cur uint64
	Max uint64
}

// enforceFallback uses prlimit64 for memory and sched_setaffinity for CPU.
func (l *LinuxLimiter) enforceFallback(pid int, limits *TaskLimits) (func(), error) {
	// Memory ceiling via prlimit64.
	//
	// This used RLIMIT_AS and was lethal (TB-11). RLIMIT_AS caps a process's
	// VIRTUAL ADDRESS SPACE, which every managed runtime reserves vastly more of
	// than it ever commits — a Go program reserves hundreds of MB of arenas
	// before main() — so capping address space at the leaf's declared memory
	// killed the process inside runtime.mallocinit(), before any leaf code ran.
	// Measured: a trivial Go binary needs between 512 MB and 768 MB of RLIMIT_AS
	// merely to START, no matter how little it goes on to use, so no headroom
	// multiple on RLIMIT_AS is defensible. This path is what ordinary
	// unprivileged Linux machines get (detectCgroupsV2 needs delegation they do
	// not have), so the entire NATIVE runtime was unusable for most volunteers.
	//
	// RLIMIT_DATA bounds the data segment and, since Linux 4.7, writable private
	// mappings — the memory a process actually commits — while leaving the
	// PROT_NONE reservations a runtime makes at startup alone. Measured: it both
	// admits a well-behaved leaf and still stops a runaway one. On kernels older
	// than 4.7 it does not cover mmap at all and so is a no-op for runtimes that
	// never call brk; that is accepted, as 4.7 predates every supported distro.
	if limitMB := fallbackMemoryLimitMB(limits.MaxMemoryMB); limitMB > 0 {
		memBytes := uint64(limitMB) * 1024 * 1024
		lim := rlimit64{Cur: memBytes, Max: memBytes}
		_, _, errno := syscall.RawSyscall6(
			syscall.SYS_PRLIMIT64,
			uintptr(pid),
			uintptr(syscall.RLIMIT_DATA),
			uintptr(unsafe.Pointer(&lim)),
			0, 0, 0,
		)
		if errno != 0 {
			l.logger.Warn("prlimit64 RLIMIT_DATA failed", "error", errno, "pid", pid)
		} else {
			l.logger.Debug("set memory limit via prlimit64",
				"pid", pid, "resource", "RLIMIT_DATA",
				"declared_mb", limits.MaxMemoryMB, "enforced_mb", limitMB)
		}
	}

	// Confine the process to the BUDGET's worth of the CPUs it is actually
	// PERMITTED to use — not to CPUs 0..N-1.
	//
	// This used to build the mask from the bare count, setting bits 0..N-1
	// unconditionally (TB-16). A process confined by a cpuset — a systemd slice
	// with AllowedCPUs=, a container run with --cpuset-cpus, an LXC/VM profile,
	// offline CPUs — is not necessarily permitted CPU 0, and sched_setaffinity(2)
	// returns EINVAL when the requested mask "contains no processors that are
	// currently physically on the system and permitted to the thread". The
	// volunteer's CPU cap was then not applied AT ALL and work ran across every
	// permitted CPU — the opposite of what they configured. Observed on a
	// tester's host failing 25 of 25 native units while a sibling host on the
	// same build succeeded 15 of 15.
	//
	// The quieter half was worse: a PARTIAL overlap SUCCEEDS at the wrong size,
	// because the kernel intersects the requested mask with the permitted set.
	// Permitted {2..7} with a 4-core budget pinned the process to {2,3} — half
	// the requested allowance — and nothing was logged at any level.
	//
	// Each task is pinned to as many of the budget's CPUs as it was granted
	//, the ones the fewest other tasks are pinned to: a task is held
	// to its grant, and the tasks together stay within the budget's CPUs.
	//
	// Note this is the fallback path only: containers get a CFS quota
	// (runtime/container.go) and cgroup-capable hosts get cpu.max above, both of
	// which cap CPU *time* and are immune to this. Affinity is the sole lever
	// left when cgroup delegation is unavailable, which is the common case on an
	// unprivileged desktop.
	if limits.CPU.BudgetCores > 0 {
		cores := limits.CPU.Cores
		if cores <= 0 || cores > limits.CPU.BudgetCores {
			cores = limits.CPU.BudgetCores
		}
		l.applyCPUAffinity(pid, limits.CPU.BudgetCores, cores)
	}

	return func() { l.unpin(pid) }, nil
}

// applyCPUAffinity pins pid to cores of the first budgetCores of its
// permitted CPUs: the ones the fewest other running tasks are pinned to, so
// each task is held to its grant and the tasks' grants spread across the
// budget's CPUs.
//
// When the process is already confined to no more than cores CPUs, the call
// is skipped: pinning could only narrow it further, which is not what a "use
// at most N cores" grant means.
func (l *LinuxLimiter) applyCPUAffinity(pid, budgetCores, cores int) {
	permitted, err := getAffinityFn(pid)
	if err != nil {
		l.logger.Warn("CPU limit not applied: could not read permitted CPUs",
			"error", err, "pid", pid, "cpu_cores", cores,
			"consequence", "work is not confined to the configured core count")
		return
	}
	if len(permitted) == 0 {
		l.logger.Warn("CPU limit not applied: no permitted CPUs reported",
			"pid", pid, "cpu_cores", cores,
			"consequence", "work is not confined to the configured core count")
		return
	}

	if len(permitted) <= cores {
		l.logger.Debug("CPU affinity left as-is: already within the task's grant",
			"pid", pid, "permitted_cpus", len(permitted), "cpu_cores", cores)
		return
	}

	budget := permitted
	if budgetCores > 0 && budgetCores < len(budget) {
		budget = budget[:budgetCores]
	}
	chosen := l.leastPinned(budget, cores)
	if err := setAffinityFn(pid, chosen); err != nil {
		l.logger.Warn("CPU limit not applied: sched_setaffinity failed",
			"error", err, "pid", pid, "cpu_cores", cores,
			"requested_cpus", chosen, "permitted_cpus", permitted,
			"consequence", "work is not confined to the configured core count")
		return
	}
	l.pinMu.Lock()
	if l.pins == nil {
		l.pins = make(map[int][]int)
	}
	l.pins[pid] = chosen
	l.pinMu.Unlock()
	l.logger.Debug("set CPU affinity", "pid", pid, "cores", len(chosen), "cpus", chosen)
}

// leastPinned picks n of cpus, the ones the fewest running tasks are pinned
// to (earlier CPUs first on a tie), in ascending order.
func (l *LinuxLimiter) leastPinned(cpus []int, n int) []int {
	if n >= len(cpus) {
		return append([]int(nil), cpus...)
	}
	l.pinMu.Lock()
	load := make(map[int]int)
	for _, pinned := range l.pins {
		for _, cpu := range pinned {
			load[cpu]++
		}
	}
	l.pinMu.Unlock()
	order := append([]int(nil), cpus...)
	sort.SliceStable(order, func(i, j int) bool { return load[order[i]] < load[order[j]] })
	chosen := order[:n]
	sort.Ints(chosen)
	return chosen
}

// unpin forgets the CPUs a task that has ended was pinned to.
func (l *LinuxLimiter) unpin(pid int) {
	l.pinMu.Lock()
	delete(l.pins, pid)
	l.pinMu.Unlock()
}

// getAffinityFn and setAffinityFn are the sched_getaffinity / sched_setaffinity
// syscalls, indirected so tests can drive applyCPUAffinity's selection logic.
// The real syscalls cannot reproduce TB-16 on an ordinary CI host — it takes a
// cpuset cgroup (a plain taskset restriction can be widened again by the process
// itself, and so does not fail) — which is exactly why the selection must be
// unit-testable away from them.
var (
	getAffinityFn = sysGetAffinity
	setAffinityFn = sysSetAffinity
)

// cpuSetSize is the affinity mask size passed to the kernel, in uint64 words.
// 1024 CPUs matches the historical mask width here. An oversized cpusetsize is
// legal — the kernel truncates to its own cpumask_size() — so this is not the
// source of the EINVAL described above.
const cpuSetWords = 1024 / 64

// sysGetAffinity returns the CPUs pid is permitted to run on, ascending.
func sysGetAffinity(pid int) ([]int, error) {
	var mask [cpuSetWords]uint64
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_GETAFFINITY,
		uintptr(pid),
		unsafe.Sizeof(mask),
		uintptr(unsafe.Pointer(&mask[0])),
	)
	if errno != 0 {
		return nil, errno
	}
	cpus := make([]int, 0, 8)
	for word := 0; word < cpuSetWords; word++ {
		if mask[word] == 0 {
			continue
		}
		for bit := 0; bit < 64; bit++ {
			if mask[word]&(1<<uint(bit)) != 0 {
				cpus = append(cpus, word*64+bit)
			}
		}
	}
	return cpus, nil
}

// sysSetAffinity pins pid to exactly the given CPUs.
func sysSetAffinity(pid int, cpus []int) error {
	var mask [cpuSetWords]uint64
	for _, cpu := range cpus {
		if cpu < 0 || cpu >= cpuSetWords*64 {
			continue
		}
		mask[cpu/64] |= 1 << (uint(cpu) % 64)
	}
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_SETAFFINITY,
		uintptr(pid),
		unsafe.Sizeof(mask),
		uintptr(unsafe.Pointer(&mask[0])),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// describeCPUEnforcement reports which of the two Linux caps is in force.
//
// A host with cgroup v2 delegation gets a CPU-time quota (cpu.max), which is
// insensitive to which CPUs the process may use. Without delegation — the norm
// on an unprivileged desktop — the only lever is the affinity mask, whose
// effective size is bounded by the permitted CPU set, so that count is what
// `doctor` must show alongside the configured limit.
func describeCPUEnforcement() CPUEnforcement {
	e := CPUEnforcement{Confinable: true}
	if detectCgroupsV2() {
		e.Mechanism = "cgroup v2 CPU quota (cpu.max)"
	} else {
		e.Mechanism = "CPU affinity (cgroup delegation unavailable)"
	}
	// pid 0 means "this thread" — doctor's own permitted set. The daemon's work
	// children inherit from the daemon, so this matches only when doctor and the
	// daemon run under the same confinement; it is a strong indicator, not proof.
	if cpus, err := getAffinityFn(0); err == nil {
		e.PermittedCPUs = len(cpus)
	}
	return e
}

// CheckDiskSpace checks available disk space on the filesystem containing path.
func (l *LinuxLimiter) CheckDiskSpace(path string, requiredMB int) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("%w: statfs %s: %v", ErrDiskSpaceUnknown, path, err)
	}

	availableMB := (stat.Bavail * uint64(stat.Bsize)) / (1024 * 1024)
	if availableMB < uint64(requiredMB) {
		return fmt.Errorf("insufficient disk space: %d MB available, %d MB required", availableMB, requiredMB)
	}

	return nil
}

// CPUThrottling reads how often a task's CPU quota stopped it, from its
// cgroup's cpu.stat. Only the cgroup path holds a task to a quota and counts
// the stops; on the affinity fallback there is no reading.
func (l *LinuxLimiter) CPUThrottling(pid int) (runtime.CPUThrottle, bool) {
	if !l.useCgroups {
		return runtime.CPUThrottle{}, false
	}
	b, err := os.ReadFile(filepath.Join(cgroupPathFor(pid), "cpu.stat"))
	if err != nil {
		return runtime.CPUThrottle{}, false
	}
	return runtime.ParseCPUStat(b)
}
