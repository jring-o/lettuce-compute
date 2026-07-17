package server

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// singletonJobsLockName is the stable identifier whose 64-bit hash is the
// advisory-lock key all replicas contend for. The same constant on every
// replica means they race for ONE lock, so exactly one becomes the singleton
// leader. (The head instance id is the leadership LOG identity, NOT the lock
// key — the key must be identical across replicas.)
const singletonJobsLockName = "lettuce:singleton-jobs"

// defaultLeadershipPollInterval bounds failover latency: a follower re-attempts
// the lock on this cadence, and the leader pings its held connection on the same
// cadence to detect a lost lock. ~15s keeps the leaderless window small while
// staying well under every reclaim/lease window it gates.
const defaultLeadershipPollInterval = 15 * time.Second

// advisoryLockKey hashes a lock name into a stable signed 64-bit key suitable
// for pg_try_advisory_lock(bigint).
func advisoryLockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64())
}

// LeadershipManager elects a single "singleton-jobs leader" among N head
// replicas using a Postgres session-level advisory lock (pg_try_advisory_lock).
// The replica that acquires the lock runs the leader-only background jobs (the
// caller's closure) under a child context; the others skip those jobs and poll
// to take over if the leader dies.
//
// CRITICAL: a session advisory lock lives only as long as the connection that
// took it. We therefore Acquire a DEDICATED *pgxpool.Conn from the pool and hold
// it for the leadership lifetime — running the lock via pool.Exec would release
// it the instant the connection returns to the pool. A crashed leader's backend
// dies, Postgres auto-releases its session lock, and a follower acquires it on
// its next poll.
type LeadershipManager struct {
	pool         *pgxpool.Pool
	logger       *slog.Logger
	lockKey      int64
	pollInterval time.Duration

	// runDone is closed once Run has returned for good (its context cancelled),
	// meaning the dedicated advisory-lock connection has been released back to
	// the pool. The shutdown tail waits on Done() before pool.Close() — closing
	// first deadlocks on that held connection (BG-32b).
	runDone  chan struct{}
	doneOnce sync.Once
}

// NewLeadershipManager builds a LeadershipManager bound to the shared pool.
func NewLeadershipManager(pool *pgxpool.Pool, logger *slog.Logger) *LeadershipManager {
	return &LeadershipManager{
		pool:         pool,
		logger:       logger,
		lockKey:      advisoryLockKey(singletonJobsLockName),
		pollInterval: defaultLeadershipPollInterval,
		runDone:      make(chan struct{}),
	}
}

// Done is closed once Run has returned with its context cancelled — i.e. the
// manager no longer holds a dedicated pool connection. If Run never reaches
// that state the channel never closes, so callers must bound their wait.
func (m *LeadershipManager) Done() <-chan struct{} {
	return m.runDone
}

// Run blocks until ctx is cancelled, repeatedly attempting to become the
// singleton leader. While this replica holds leadership it runs runLeaderJobs
// under a child context; on lock loss or ctx cancellation that child context is
// cancelled so the leader jobs stop cleanly. instanceID is the leadership log
// identity (not the lock key).
//
// runLeaderJobs is invoked once each time leadership is (re)acquired and is
// expected to START the leader-only background goroutines on the context it is
// handed and return promptly (it must NOT block). Those goroutines must observe
// the context's cancellation to stop when leadership is lost.
func (m *LeadershipManager) Run(ctx context.Context, instanceID string, runLeaderJobs func(leaderCtx context.Context)) {
	defer func() {
		// Signal shutdown-join completion only when Run is done for good (ctx
		// cancelled). A panic mid-run unwinds through here too, but its restart
		// (safego) may hold a fresh lock connection — the ctx guard keeps Done()
		// honest in that case.
		if ctx.Err() != nil {
			m.doneOnce.Do(func() { close(m.runDone) })
		}
	}()
	m.logger.Info("leadership manager starting", "instance_id", instanceID, "poll_interval", m.pollInterval)

	for {
		// Try to become leader. acquireLeadership blocks until leadership is lost
		// or ctx is cancelled when it succeeds; on failure it returns immediately.
		acquired := m.acquireAndHold(ctx, instanceID, runLeaderJobs)
		if ctx.Err() != nil {
			m.logger.Info("leadership manager stopping", "instance_id", instanceID)
			return
		}
		if !acquired {
			// Follower: wait a poll interval, then re-attempt.
			select {
			case <-ctx.Done():
				m.logger.Info("leadership manager stopping", "instance_id", instanceID)
				return
			case <-time.After(m.pollInterval):
			}
		}
		// If acquired was true we have just lost leadership; loop and re-contend
		// immediately (another replica may already hold it, in which case the next
		// attempt falls into the follower wait above).
	}
}

// acquireAndHold acquires a dedicated connection, attempts the advisory lock,
// and — if it wins — runs the leader jobs and blocks until the lock is lost or
// ctx is cancelled. Returns true if leadership was acquired (and has since been
// released), false if the lock was already held by another replica or an error
// prevented acquisition.
func (m *LeadershipManager) acquireAndHold(ctx context.Context, instanceID string, runLeaderJobs func(context.Context)) bool {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Error("leadership: failed to acquire dedicated connection", "instance_id", instanceID, "error", err)
		}
		return false
	}
	// From here a single exit path releases the connection.
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", m.lockKey).Scan(&locked); err != nil {
		if ctx.Err() == nil {
			m.logger.Error("leadership: pg_try_advisory_lock failed", "instance_id", instanceID, "error", err)
		}
		return false
	}
	if !locked {
		// Another replica is the leader.
		return false
	}

	m.logger.Info("became singleton leader", "instance_id", instanceID)

	// Start leader-only jobs under a child context that we cancel the moment
	// leadership is lost or the parent context is cancelled, so leader goroutines
	// never leak past leadership.
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	defer cancelLeader()
	runLeaderJobs(leaderCtx)

	// Hold the lock: ping the held connection on the poll cadence. A failed ping
	// means the connection (and thus the session lock) is gone — relinquish
	// leadership so a follower can take over.
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.releaseLock(conn, instanceID)
			return true
		case <-ticker.C:
			if err := conn.Ping(ctx); err != nil {
				if ctx.Err() == nil {
					m.logger.Warn("leadership: lost lock connection, relinquishing leadership", "instance_id", instanceID, "error", err)
				}
				// The connection is broken; the session lock is already released
				// by Postgres. Cancel leader jobs (via defer) and return.
				return true
			}
		}
	}
}

// releaseLock best-effort unlocks the advisory lock on a still-healthy
// connection during graceful shutdown. On a broken connection the lock is
// already released by Postgres, so errors here are non-fatal.
func (m *LeadershipManager) releaseLock(conn *pgxpool.Conn, instanceID string) {
	// Use a short, independent context so unlock still runs during shutdown when
	// the parent context is already cancelled.
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", m.lockKey); err != nil {
		m.logger.Warn("leadership: advisory unlock failed (lock auto-released on connection close)", "instance_id", instanceID, "error", err)
		return
	}
	m.logger.Info("released singleton leadership", "instance_id", instanceID)
}
