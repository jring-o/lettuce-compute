//go:build windows

package resource

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procLastInput    = user32.NewProc("GetLastInputInfo")
	procGetTickCount = kernel32W.NewProc("GetTickCount") // kernel32W declared in limiter_windows.go
)

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

// GetIdleSeconds returns the number of seconds since the last user input
// on Windows, using GetLastInputInfo from user32.dll.
func GetIdleSeconds() (int, error) {
	info := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	ret, _, err := procLastInput.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return 0, fmt.Errorf("GetLastInputInfo: %w", err)
	}

	tickCount, _, _ := procGetTickCount.Call()
	idleMs := uint32(tickCount) - info.dwTime
	return int(idleMs / 1000), nil
}
