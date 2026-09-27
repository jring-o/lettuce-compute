package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The engine client's stats reading: the CPU counters and working set of a
// running container, and errContainerNotRunning — never a zero reading — once
// it has exited, whichever way the engine says so.
func TestContainerUsageReadsRunningContainerAndRefusesStoppedOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		podman bool
	}{
		{"docker", false},
		{"podman", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fe, srv := newFakeEngine(t, tc.podman, 300*time.Millisecond)
			fe.adopt("c1", time.Minute)
			cr := newFakeEngineRuntime(t, srv)
			ctx := context.Background()

			s, err := cr.Client().ContainerUsage(ctx, "c1")
			if err != nil {
				t.Fatalf("reading a running container: %v", err)
			}
			if s.CPUUsageUser == 0 || s.CPUUsageKernel == 0 {
				t.Errorf("CPU counters = %d user, %d kernel; want the engine's figures", s.CPUUsageUser, s.CPUUsageKernel)
			}
			if want := uint64(fakeEngineWorkMB * 1024 * 1024); s.MemoryBytes != want {
				t.Errorf("MemoryBytes = %d, want %d (the working set without the page cache)", s.MemoryBytes, want)
			}

			time.Sleep(400 * time.Millisecond)
			if s, err := cr.Client().ContainerUsage(ctx, "c1"); !errors.Is(err, errContainerNotRunning) {
				t.Errorf("reading an exited container = %+v, %v; want errContainerNotRunning", s, err)
			}
		})
	}
}

func TestContainerUsageAccumulation(t *testing.T) {
	const s = uint64(time.Second)
	reading := func(userSec, kernelSec, memMB uint64) *ContainerStats {
		return &ContainerStats{CPUUsageUser: userSec * s, CPUUsageKernel: kernelSec * s, MemoryBytes: memMB << 20}
	}

	t.Run("no reading reports nothing", func(t *testing.T) {
		u := &containerUsage{}
		if _, _, ok := u.cpu(); ok {
			t.Error("CPU reported without a reading")
		}
		if u.peakMemory() != 0 {
			t.Errorf("peak = %d without a reading", u.peakMemory())
		}
	})

	t.Run("latest CPU and highest memory", func(t *testing.T) {
		u := &containerUsage{}
		u.add(reading(10, 1, 400))
		u.add(reading(20, 2, 150))
		u.add(reading(5, 0, 300)) // a lower CPU reading never replaces a higher one
		user, kernel, ok := u.cpu()
		if !ok || user != 20*s || kernel != 2*s {
			t.Errorf("cpu = %d, %d, %v; want 20 s, 2 s", user, kernel, ok)
		}
		if u.peakMemory() != 400<<20 {
			t.Errorf("peak = %d MB, want 400", u.peakMemory()>>20)
		}
	})

	t.Run("adopted container counts from its baseline", func(t *testing.T) {
		u := &containerUsage{}
		u.setBaseline(reading(18000, 900, 200))
		u.add(reading(18012, 901, 250))
		user, kernel, ok := u.cpu()
		if !ok || user != 12*s || kernel != 1*s {
			t.Errorf("cpu = %d, %d, %v; want 12 s, 1 s", user, kernel, ok)
		}
	})

	t.Run("adopted container without a baseline reports no CPU", func(t *testing.T) {
		u := &containerUsage{}
		u.setBaseline(nil)
		u.add(reading(18012, 901, 250))
		if _, _, ok := u.cpu(); ok {
			t.Error("CPU reported although the adopted container's starting counters are unknown")
		}
		if u.peakMemory() != 250<<20 {
			t.Errorf("peak = %d MB, want 250 (memory needs no baseline)", u.peakMemory()>>20)
		}
	})
}

// Against a REAL engine: its answer for an exited container reads as "not
// running", never as a zero reading. Gated (LETTUCE_TEST_REAL_ENGINE=1).
func TestRealEngine_ContainerUsageOfExitedContainer(t *testing.T) {
	cr := newRealEngineRuntime(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dc := cr.Client()
	image := "docker.io/library/alpine:3.20"
	if err := dc.ImagePull(ctx, image); err != nil {
		t.Skipf("cannot pull %s: %v", image, err)
	}
	id, err := dc.ContainerCreate(ctx, &ContainerConfig{
		Image: image, Cmd: []string{"true"}, NetworkMode: "none",
		Labels:  map[string]string{WorkUnitIDLabel: "usage-exited", DataDirLabel: cr.dataDir},
		Backend: cr.backend,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer dc.ContainerRemove(context.Background(), id)
	if err := dc.ContainerStart(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := dc.ContainerWait(ctx, id); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if s, err := dc.ContainerUsage(ctx, id); !errors.Is(err, errContainerNotRunning) {
		t.Errorf("stats of an exited container = %+v, %v; want errContainerNotRunning", s, err)
	}
}
