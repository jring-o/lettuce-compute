package management

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	lettucev1 "github.com/lettuce-compute/infrastructure/proto/lettuce/v1"
	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// TestTB57_LocalCreditBucketsByTheLocalDay is the tester's night: four units
// completed in the 23:00Z hour of 2026-08-31 — 01:06 to 01:59 on 09-01 in his
// UTC+2 — then forty-four more through 09-01. The history list groups all
// forty-eight under Today; the counters said Today = 44 because they cut the day
// at 00:00Z. Both surfaces run on the same machine, so the local fallback must cut
// the day by that machine's clock: today = 48.
func TestTB57_LocalCreditBucketsByTheLocalDay(t *testing.T) {
	cest := time.FixedZone("CEST", 2*3600)
	now := time.Date(2026, 9, 1, 14, 45, 0, 0, cest)

	var entries []daemon.HistoryEntry
	first := time.Date(2026, 8, 31, 23, 6, 21, 0, time.UTC) // 01:06:21 CEST on 09-01
	for i := 0; i < 4; i++ {
		entries = append(entries, daemon.HistoryEntry{
			WorkUnitID: "night", LeafID: "leaf", ResultAccepted: true,
			CompletedAt: first.Add(time.Duration(i) * 12 * time.Minute),
		})
	}
	day := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	for i := 0; i < 44; i++ {
		entries = append(entries, daemon.HistoryEntry{
			WorkUnitID: "day", LeafID: "leaf", ResultAccepted: true,
			CompletedAt: day.Add(time.Duration(i) * 15 * time.Minute),
		})
	}

	b := bucketAcceptedHistory(entries, now)
	if b.total != 48 {
		t.Fatalf("total = %v, want 48", b.total)
	}
	if b.today != 48 {
		t.Errorf("today = %v, want 48: every unit completed after this machine's midnight is today, as the history list already says (TB-57)", b.today)
	}

	// The rule follows the caller's clock: a machine on UTC still sees the UTC day.
	if bu := bucketAcceptedHistory(entries, now.UTC()); bu.today != 44 {
		t.Errorf("today in UTC = %v, want 44", bu.today)
	}

	// A unit just before local midnight is yesterday, not today.
	late := []daemon.HistoryEntry{{LeafID: "leaf", ResultAccepted: true,
		CompletedAt: time.Date(2026, 8, 31, 23, 59, 0, 0, cest)}}
	if bl := bucketAcceptedHistory(late, now); bl.today != 0 || bl.total != 1 {
		t.Errorf("23:59 local yesterday: today = %v total = %v, want 0 and 1", bl.today, bl.total)
	}
}

// TestTB57_CreditSummaryStatesItsDayBoundary: the API must say which calendar it
// bucketed by, so the app can label the counters instead of leaving "today" to be
// read against a local-day history list. Head-derived buckets are UTC (the head's
// daily timeline is by UTC date); the local fallback is this machine's day.
func TestTB57_CreditSummaryStatesItsDayBoundary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	t.Run("head-derived buckets are utc", func(t *testing.T) {
		d, dir := tb46Daemon(t)
		mockClient := &e2eMockWorkClient{
			getMyContributionFn: func(ctx context.Context, req *lettucev1.GetMyContributionRequest) (*lettucev1.GetMyContributionResponse, error) {
				return &lettucev1.GetMyContributionResponse{VolunteerId: "vol-1", TotalCredit: 3}, nil
			},
		}
		d.SetMultiClientForTest(daemon.NewMultiServerClient([]*daemon.ServerConnection{
			{Name: "head-alpha", VolunteerID: "vol-1", Available: true, Client: mockClient},
		}, logger))

		summary := NewDaemonBridge(d, filepath.Join(dir, "config.yaml")).GetCredit()
		if summary.Source != "head" {
			t.Fatalf("source = %q, want head", summary.Source)
		}
		if summary.DayBoundary != "utc" {
			t.Errorf("day_boundary = %q, want utc for head-derived buckets (TB-57)", summary.DayBoundary)
		}
	})

	t.Run("local fallback buckets are local", func(t *testing.T) {
		d, dir := tb46Daemon(t)
		daemon.AppendHistory(dir, daemon.HistoryEntry{
			WorkUnitID: "wu-1", LeafID: "leaf", CompletedAt: time.Now().UTC(), ResultAccepted: true,
		})

		summary := NewDaemonBridge(d, filepath.Join(dir, "config.yaml")).GetCredit()
		if summary.Source != "local" {
			t.Fatalf("source = %q, want local", summary.Source)
		}
		if summary.DayBoundary != "local" {
			t.Errorf("day_boundary = %q, want local for history-derived buckets (TB-57)", summary.DayBoundary)
		}
		if summary.Today != 1 {
			t.Errorf("today = %v, want 1 for a unit completed just now", summary.Today)
		}
	})
}
