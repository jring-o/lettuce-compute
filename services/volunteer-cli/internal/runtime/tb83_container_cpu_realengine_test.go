package runtime

import (
	"context"
	"testing"
	"time"
)

// TB-83, against a REAL engine: the one-shot stats read the yield monitor
// uses to subtract Lettuce's own containers from the machine's load must
// return a cumulative CPU figure from Podman's compatibility API as well as
// Docker's, and must report a removed container as not found rather than as
// a failure. Gated like the other real-engine tests (LETTUCE_TEST_REAL_ENGINE=1).
func TestTB83_RealEngine_ContainerCPUNanos(t *testing.T) {
	rt := newRealEngineRuntime(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cli := rt.Client()
	image := "docker.io/library/alpine:3.20"
	if err := cli.ImagePull(ctx, image); err != nil {
		t.Skipf("cannot pull %s: %v", image, err)
	}
	id, err := cli.ContainerCreate(ctx, &ContainerConfig{
		Image:   image,
		Cmd:     []string{"sh", "-c", "i=0; while [ $i -lt 400000 ]; do i=$((i+1)); done; sleep 30"},
		Labels:  map[string]string{"lettuce.test": "tb83"},
		Backend: rt.backend,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		_ = cli.ContainerRemove(context.Background(), id)
	}()
	if err := cli.ContainerStart(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Give the busy loop a moment, then the counter must be positive and
	// must not decrease between two reads.
	time.Sleep(2 * time.Second)
	first, err := cli.ContainerCPUNanos(ctx, id)
	if err != nil {
		t.Fatalf("ContainerCPUNanos: %v", err)
	}
	if first == 0 {
		t.Error("CPU nanoseconds = 0 after two seconds of a busy loop; the engine's stats are not being read")
	}
	time.Sleep(time.Second)
	second, err := cli.ContainerCPUNanos(ctx, id)
	if err != nil {
		t.Fatalf("second ContainerCPUNanos: %v", err)
	}
	if second < first {
		t.Errorf("CPU nanoseconds fell from %d to %d; the figure must be cumulative", first, second)
	}

	// A container that is gone is "not found", which the daemon treats as a
	// finished task, not as a measurement failure.
	if err := cli.ContainerStop(ctx, id, 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := cli.ContainerRemove(ctx, id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	_, err = cli.ContainerCPUNanos(ctx, id)
	if !IsContainerNotFound(err) {
		t.Errorf("stats of a removed container: err = %v, want a not-found error", err)
	}
}
