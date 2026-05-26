package resource

import "testing"

func TestGetIdleSeconds_NonNegative(t *testing.T) {
	seconds, err := GetIdleSeconds()
	if err != nil {
		t.Skipf("idle detection not available on this platform: %v", err)
	}
	if seconds < 0 {
		t.Errorf("GetIdleSeconds() = %d, want >= 0", seconds)
	}
}
