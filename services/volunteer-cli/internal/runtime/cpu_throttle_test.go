package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestParseCPUStat reads the two counts from a cgroup v2 cpu.stat and a v1
// one, and refuses a file that lacks either.
func TestParseCPUStat(t *testing.T) {
	v2 := []byte("usage_usec 13460283\nuser_usec 13436218\nsystem_usec 24064\nnr_periods 135\nnr_throttled 134\nthrottled_usec 40160579\nnr_bursts 0\nburst_usec 0\n")
	if got, ok := ParseCPUStat(v2); !ok || got != (CPUThrottle{Periods: 135, Throttled: 134}) {
		t.Errorf("cgroup v2 cpu.stat: %+v %v, want 135/134", got, ok)
	}
	v1 := []byte("nr_periods 40\nnr_throttled 2\nthrottled_time 1200000\n")
	if got, ok := ParseCPUStat(v1); !ok || got != (CPUThrottle{Periods: 40, Throttled: 2}) {
		t.Errorf("cgroup v1 cpu.stat: %+v %v, want 40/2", got, ok)
	}
	if _, ok := ParseCPUStat([]byte("usage_usec 10\n")); ok {
		t.Error("a cpu.stat with no period counts parsed as a reading")
	}
}

// TestContainerCPUThrottlingSources: an engine that reports throttling in its
// stats (Docker) is read there, with nothing run inside the container; one
// that sends zeros (Podman) is read from the container's own cpu.stat,
// cgroup v2 first and then v1's; with neither there is no reading.
func TestContainerCPUThrottlingSources(t *testing.T) {
	ctx := context.Background()

	var execs [][]string
	docker := &MockDockerClient{
		ContainerUsageFn: func(context.Context, string) (*ContainerStats, error) {
			return &ContainerStats{ThrottlePeriods: 600, ThrottledPeriods: 450}, nil
		},
		ContainerExecOutputFn: func(_ context.Context, _ string, cmd []string) ([]byte, error) {
			execs = append(execs, cmd)
			return nil, errors.New("not expected")
		},
	}
	if got, err := ContainerCPUThrottling(ctx, docker, "c1"); err != nil || got != (CPUThrottle{Periods: 600, Throttled: 450}) {
		t.Errorf("from the engine's stats: %+v %v, want 600/450", got, err)
	}
	if len(execs) != 0 {
		t.Errorf("ran %v inside the container although the stats answered", execs)
	}

	podman := &MockDockerClient{
		ContainerUsageFn: func(context.Context, string) (*ContainerStats, error) {
			return &ContainerStats{}, nil
		},
		ContainerExecOutputFn: func(_ context.Context, _ string, cmd []string) ([]byte, error) {
			execs = append(execs, cmd)
			if cmd[1] == "/sys/fs/cgroup/cpu.stat" {
				return nil, errors.New("cat: can't open '/sys/fs/cgroup/cpu.stat': No such file or directory")
			}
			return []byte("nr_periods 135\nnr_throttled 134\nthrottled_time 1\n"), nil
		},
	}
	if got, err := ContainerCPUThrottling(ctx, podman, "c2"); err != nil || got != (CPUThrottle{Periods: 135, Throttled: 134}) {
		t.Errorf("from the container's cpu.stat: %+v %v, want 135/134", got, err)
	}
	want := [][]string{{"cat", "/sys/fs/cgroup/cpu.stat"}, {"cat", "/sys/fs/cgroup/cpu/cpu.stat"}}
	if !reflect.DeepEqual(execs, want) {
		t.Errorf("ran %v inside the container, want %v", execs, want)
	}

	none := &MockDockerClient{
		ContainerUsageFn: func(context.Context, string) (*ContainerStats, error) { return &ContainerStats{}, nil },
		ContainerExecOutputFn: func(context.Context, string, []string) ([]byte, error) {
			return nil, errors.New("exec: \"cat\": executable file not found in $PATH")
		},
	}
	if _, err := ContainerCPUThrottling(ctx, none, "c3"); err == nil {
		t.Error("an image with no cat and an engine with no throttling stats gave a reading")
	}
}
