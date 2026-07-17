package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// BG-34 regression: the production compose now runs Redis with --requirepass
// and hands the head a redis://:password@host URL. NewRedisClient must carry
// the credential through (redis.ParseURL) so the right password connects, and
// must fail closed — at construction, via its verification PING — on a wrong
// or absent password rather than returning a client that errors on first use.
func TestNewRedisClient_PasswordAuth_BG34(t *testing.T) {
	srv := miniredis.RunT(t)
	srv.RequireAuth("correct-password")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("correct password connects", func(t *testing.T) {
		client, err := NewRedisClient(ctx, fmt.Sprintf("redis://:correct-password@%s", srv.Addr()))
		if err != nil {
			t.Fatalf("expected authenticated connect to succeed, got: %v", err)
		}
		defer client.Close()
		if err := client.Ping(ctx).Err(); err != nil {
			t.Fatalf("authenticated client failed to ping: %v", err)
		}
	})

	t.Run("wrong password fails", func(t *testing.T) {
		client, err := NewRedisClient(ctx, fmt.Sprintf("redis://:wrong-password@%s", srv.Addr()))
		if err == nil {
			client.Close()
			t.Fatal("expected connect with a wrong password to fail, got nil error")
		}
	})

	t.Run("absent password fails", func(t *testing.T) {
		client, err := NewRedisClient(ctx, fmt.Sprintf("redis://%s", srv.Addr()))
		if err == nil {
			client.Close()
			t.Fatal("expected unauthenticated connect to fail against a password-protected server, got nil error")
		}
	})
}
