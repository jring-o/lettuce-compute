package runtime

import (
	"context"
	"errors"
	"testing"
)

// TestContainerRuntime_PullPolicy verifies the TODO #38 pull policy: a digest-pinned
// ref is content-addressed (pull only on a cache miss), while a tag ref (which may be
// a re-pushed :latest) is refreshed on every prepare — but falls back to a cached
// image when the refresh pull fails, so a registry outage can't break a runnable unit.
func TestContainerRuntime_PullPolicy(t *testing.T) {
	const digestRef = "lbry.science/beyblade@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"
	const tagRef = "lbry.science/beyblade:latest"

	t.Run("digest ref cached: no re-pull", func(t *testing.T) {
		pulls := 0
		mock := &MockDockerClient{
			ImageExistsFn: func(ctx context.Context, ref string) (bool, error) { return true, nil },
			ImagePullFn:   func(ctx context.Context, ref string) error { pulls++; return nil },
		}
		cr, _ := newTestContainerRuntime(t, mock)
		wu := &WorkUnit{ID: "11111111-1111-1111-1111-111111111111", ExecutionSpec: ExecutionSpec{Image: digestRef}}
		prep, err := cr.Prepare(context.Background(), wu)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		defer cr.Cleanup(prep)
		if pulls != 0 {
			t.Errorf("digest + cached: expected 0 pulls, got %d", pulls)
		}
	})

	t.Run("tag ref cached: refresh pull attempted", func(t *testing.T) {
		pulls := 0
		mock := &MockDockerClient{
			ImageExistsFn: func(ctx context.Context, ref string) (bool, error) { return true, nil },
			ImagePullFn:   func(ctx context.Context, ref string) error { pulls++; return nil },
		}
		cr, _ := newTestContainerRuntime(t, mock)
		wu := &WorkUnit{ID: "22222222-2222-2222-2222-222222222222", ExecutionSpec: ExecutionSpec{Image: tagRef}}
		prep, err := cr.Prepare(context.Background(), wu)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		defer cr.Cleanup(prep)
		if pulls != 1 {
			t.Errorf("tag + cached: expected 1 refresh pull, got %d", pulls)
		}
	})

	t.Run("tag ref cached, pull fails: falls back to cached image", func(t *testing.T) {
		mock := &MockDockerClient{
			ImageExistsFn: func(ctx context.Context, ref string) (bool, error) { return true, nil },
			ImagePullFn:   func(ctx context.Context, ref string) error { return errors.New("registry unreachable") },
		}
		cr, _ := newTestContainerRuntime(t, mock)
		wu := &WorkUnit{ID: "33333333-3333-3333-3333-333333333333", ExecutionSpec: ExecutionSpec{Image: tagRef}}
		prep, err := cr.Prepare(context.Background(), wu)
		if err != nil {
			t.Fatalf("expected fallback to cached image, got error: %v", err)
		}
		cr.Cleanup(prep)
	})

	t.Run("tag ref not cached, pull fails: hard error", func(t *testing.T) {
		mock := &MockDockerClient{
			ImageExistsFn: func(ctx context.Context, ref string) (bool, error) { return false, nil },
			ImagePullFn:   func(ctx context.Context, ref string) error { return errors.New("not found") },
		}
		cr, _ := newTestContainerRuntime(t, mock)
		wu := &WorkUnit{ID: "44444444-4444-4444-4444-444444444444", ExecutionSpec: ExecutionSpec{Image: tagRef}}
		if _, err := cr.Prepare(context.Background(), wu); err == nil {
			t.Error("expected a hard error when an uncached tag pull fails")
		}
	})
}
