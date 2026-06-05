//go:build integration

package server

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"
)

// redisURLForTest returns the Redis URL for the integration test, defaulting to a
// throwaway podman redis on port 6390 (see the L3 test recipe). Skips the test when
// LETTUCE_TEST_REDIS_URL is explicitly set to "" to allow opting out.
func redisURLForTest(t *testing.T) string {
	t.Helper()
	url := os.Getenv("LETTUCE_TEST_REDIS_URL")
	if url == "" {
		url = "redis://127.0.0.1:6390"
	}
	return url
}

// TestRedisReplayStore_Integration proves the real Redis-backed replay store
// dedups GLOBALLY across two store instances (two replicas) sharing one Redis: a
// signature recorded by replica A is reported as a replay by replica B. This is the
// BREAK-2 cross-replica replay rejection against a live Redis (SET NX PX).
func TestRedisReplayStore_Integration(t *testing.T) {
	ctx := context.Background()
	client, err := NewRedisClient(ctx, redisURLForTest(t))
	if err != nil {
		t.Skipf("redis unavailable for integration test: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Two independent stores sharing ONE Redis client = two replicas.
	replicaA := newRedisReplayStore(client)
	replicaB := newRedisReplayStore(client)

	// Unique signature per run so repeated runs don't collide within the TTL.
	sig := make([]byte, 64)
	if _, err := rand.Read(sig); err != nil {
		t.Fatalf("rand: %v", err)
	}

	seen, err := replicaA.SeenWithin(ctx, sig, ed25519TimestampSkew)
	if err != nil {
		t.Fatalf("replica A SeenWithin: %v", err)
	}
	if seen {
		t.Fatal("first sight on replica A should be new")
	}

	seen, err = replicaB.SeenWithin(ctx, sig, ed25519TimestampSkew)
	if err != nil {
		t.Fatalf("replica B SeenWithin: %v", err)
	}
	if !seen {
		t.Fatal("replica B must reject the cross-replica replay (already-seen) via Redis")
	}
}

// TestSharedRateLimit_Integration proves the real Redis-backed shared bucket grants
// exactly the configured GLOBAL budget across two replicas sharing one Redis key.
func TestSharedRateLimit_Integration(t *testing.T) {
	ctx := context.Background()
	client, err := NewRedisClient(ctx, redisURLForTest(t))
	if err != nil {
		t.Skipf("redis unavailable for integration test: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Unique key per run so repeated runs don't share a window.
	keyBytes := make([]byte, 8)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("rand: %v", err)
	}
	clientKey := "grpc:test-" + time.Now().Format("150405.000000000")

	limit := 5
	replicaA := newSharedBucket(client, clientKey, limit, time.Minute)
	replicaB := newSharedBucket(client, clientKey, limit, time.Minute)

	now := time.Now()
	allowed := 0
	for i := 0; i < limit*2; i++ {
		b := replicaA
		if i%2 == 1 {
			b = replicaB
		}
		if ok, _, _ := b.allow(now); ok {
			allowed++
		}
	}
	if allowed != limit {
		t.Fatalf("shared Redis budget should allow exactly %d across replicas, allowed %d", limit, allowed)
	}
}
