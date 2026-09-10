//go:build windows

package runtime

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32CPU        = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes = kernel32CPU.NewProc("GetSystemTimes")
)

// NewMachineCPUSampler reads whole-machine CPU time from GetSystemTimes: the
// idle, kernel and user time of all processors since boot, in 100 ns units.
// Kernel time includes idle time, so busy is (kernel − idle) + user.
func NewMachineCPUSampler() MachineCPUSampler {
	return &deltaSampler{read: readSystemTimes}
}

func readSystemTimes() (cpuTimes, error) {
	var idle, kernel, user windows.Filetime
	ret, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return cpuTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	i, k, u := filetimeTicks(idle), filetimeTicks(kernel), filetimeTicks(user)
	total := k + u
	return cpuTimes{busy: total - i, total: total}, nil
}

func filetimeTicks(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// SelfCPUSeconds is the CPU time this process has used so far (kernel +
// user), from GetProcessTimes on the current process.
func SelfCPUSeconds() (float64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0, fmt.Errorf("GetProcessTimes: %w", err)
	}
	return float64(filetimeTicks(kernel)+filetimeTicks(user)) / 1e7, nil
}
