package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeSetNXRedis is an in-memory fake implementing the narrow replaySetNXCmd
// surface (SetNX). It models Redis NX semantics: the first SetNX for a key succeeds
// (returns true), subsequent ones return false. When failErr is set, SetNX returns
// it (to test fail-open / fail-closed at the call site).
type fakeSetNXRedis struct {
	mu      sync.Mutex
	keys    map[string]struct{}
	failErr error
}

func newFakeSetNXRedis() *fakeSetNXRedis {
	return &fakeSetNXRedis{keys: make(map[string]struct{})}
}

func (f *fakeSetNXRedis) SetNX(_ context.Context, key string, _ interface{}, _ time.Duration) *redis.BoolCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := redis.NewBoolCmd(context.Background())
	if f.failErr != nil {
		cmd.SetErr(f.failErr)
		return cmd
	}
	if _, exists := f.keys[key]; exists {
		cmd.SetVal(false) // NX failed: key already present
		return cmd
	}
	f.keys[key] = struct{}{}
	cmd.SetVal(true) // newly set
	return cmd
}

// TestRedisReplayStore_GlobalDedup verifies the Redis-backed store reports a
// signature as new on first sight (across the GLOBAL key) and already-seen on a
// replay, and that two stores sharing ONE fake (two replicas) see the same key — the
// cross-replica replay guarantee.
func TestRedisReplayStore_GlobalDedup(t *testing.T) {
	fake := newFakeSetNXRedis()
	replicaA := newRedisReplayStore(fake)
	replicaB := newRedisReplayStore(fake)

	sig := []byte("sig-CCCC")

	seen, err := replicaA.SeenWithin(context.Background(), sig, ed25519TimestampSkew)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen {
		t.Fatal("first sight on replica A should be new")
	}

	// Replica B sees the SAME global key and must report a replay.
	seen, err = replicaB.SeenWithin(context.Background(), sig, ed25519TimestampSkew)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !seen {
		t.Fatal("replica B must report cross-replica replay (already-seen)")
	}
}

// TestRedisReplayStore_ErrorSurfaced verifies a store error is returned to the
// caller (so the caller's fail-open/closed policy decides) rather than being
// swallowed as alreadySeen.
func TestRedisReplayStore_ErrorSurfaced(t *testing.T) {
	fake := newFakeSetNXRedis()
	fake.failErr = errors.New("simulated outage")
	store := newRedisReplayStore(fake)

	seen, err := store.SeenWithin(context.Background(), []byte("sig"), ed25519TimestampSkew)
	if err == nil {
		t.Fatal("expected store error to be surfaced")
	}
	if seen {
		t.Fatal("on error, alreadySeen must be false (never fabricate a replay)")
	}
}
