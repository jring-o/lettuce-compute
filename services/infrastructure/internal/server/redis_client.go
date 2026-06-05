package server

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient builds a Redis client from a redis:// (or rediss://) URL and
// verifies connectivity with a short PING. The caller owns Close(). It is used to
// back the cross-replica shared replay store and the shared rate-limit store
// (Layer 3). An empty URL is a programming error here; callers must check for it
// first (an empty RedisURL means single-replica in-mem behavior, no client).
func NewRedisClient(ctx context.Context, redisURL string) (*redis.Client, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(opt)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return client, nil
}

// grpcSharedReplayStore, when non-nil, is the cross-replica replay store the gRPC
// auth interceptor uses. It is installed once at startup by SetSharedReplayStore
// (mirroring the SetGRPCRateLimits / SetRateLimitRedisClient setter pattern) and
// read by NewGRPCServer when no explicit store is passed. nil = a fresh in-process
// in-mem store (single-replica behavior). The unexported replayStore type cannot be
// named from package main, so a setter is the injection seam rather than threading
// the value through NewGRPCServer's variadic from main.
var grpcSharedReplayStore replayStore

// SetSharedReplayStore installs the Redis-backed cross-replica replay store for
// BOTH the gRPC auth path (consumed by NewGRPCServer) and the HTTP/REST auth path
// (ed25519ReplayStore). Call once at startup, before serving. A signature accepted
// by one replica is then rejected by every replica (key = signature alone, GLOBAL).
func SetSharedReplayStore(client redis.Cmdable) {
	store := newRedisReplayStore(client)
	grpcSharedReplayStore = store
	SetEd25519ReplayStore(store)
}
