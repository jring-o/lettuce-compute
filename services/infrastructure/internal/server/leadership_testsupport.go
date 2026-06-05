//go:build integration

package server

import "time"

// SetLeadershipPollIntervalForTest shrinks a LeadershipManager's poll/ping
// cadence so leadership-failover tests complete quickly. Integration-only.
func SetLeadershipPollIntervalForTest(m *LeadershipManager, d time.Duration) {
	m.pollInterval = d
}
