//go:build windows

package client

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// defaultDetectCPUModel reads the CPU model from the Windows registry
// (HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0\ProcessorNameString).
//
// This deliberately avoids invoking `wmic cpu get Name` (or any other WMI
// query). On modern Windows hosts, `wmic` is deprecated and certain WMI
// providers (notably storage-adjacent classes) cause Windows to surface
// DiskPart.exe UAC elevation prompts at process launch. The registry
// path is readable without elevation and incurs no process creation, so
// it cannot trigger UAC.
func defaultDetectCPUModel() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`,
		registry.QUERY_VALUE)
	if err != nil {
		return "unknown"
	}
	defer k.Close()

	name, _, err := k.GetStringValue("ProcessorNameString")
	if err != nil {
		return "unknown"
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	return name
}

func defaultDetectTotalMemoryMB() int32 {
	type memoryStatusEx struct {
		length               uint32
		memoryLoad           uint32
		totalPhys            uint64
		availPhys            uint64
		totalPageFile        uint64
		availPageFile        uint64
		totalVirtual         uint64
		availVirtual         uint64
		availExtendedVirtual uint64
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GlobalMemoryStatusEx")

	var ms memoryStatusEx
	ms.length = uint32(unsafe.Sizeof(ms))
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&ms)))
	if ret == 0 {
		return 0
	}
	return int32(ms.totalPhys / (1024 * 1024))
}

func defaultDetectDiskAvailableMB(path string) int64 {
	if path == "" {
		path = "."
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}

	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	ret, _, _ := proc.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if ret == 0 {
		return 0
	}
	return int64(freeBytesAvailable / (1024 * 1024))
}
