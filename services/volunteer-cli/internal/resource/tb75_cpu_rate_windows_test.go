//go:build windows

package resource

import "testing"

// TB-75 regression test, Windows limiter half: the Job Object's CPU
// rate is the task's grant as a fraction of the machine.

// TestTB75_CPURateIsTheGrantOfTheMachine: 2 cores of a 4-CPU machine is 50 %
// → 5000; the API's 1 %–100 % range clamps a tiny or oversized grant.
func TestTB75_CPURateIsTheGrantOfTheMachine(t *testing.T) {
	cases := []struct {
		cores, numCPU int
		want          uint32
	}{
		{2, 4, 5000}, {1, 8, 1250}, {3, 8, 3750}, {1, 128, 100}, {16, 8, 10000},
	}
	for _, c := range cases {
		if got := cpuRateFor(c.cores, c.numCPU); got != c.want {
			t.Errorf("cpuRateFor(%d, %d) = %d, want %d", c.cores, c.numCPU, got, c.want)
		}
	}
}
