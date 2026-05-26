//go:build !linux

package daemon

// defaultFreeSystemMemoryMB has no portable implementation outside Linux, so it
// reports "unavailable" and admission falls back to the configured memory
// budget. Volunteers running memory-bound container leaves are Linux/amd64.
func defaultFreeSystemMemoryMB() (int, bool) {
	return 0, false
}
