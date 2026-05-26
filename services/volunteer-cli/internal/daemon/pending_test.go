package daemon

import (
	"testing"
	"time"
)

func TestSavePendingResult_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	pr := PendingResult{
		WorkUnitID:       "dc5ff9da-f084-4dd7-86b8-e829669814f8",
		LeafID:           "leaf-1",
		ServerName:       "head-a",
		RequestProto:     []byte{0x0a, 0x04, 't', 'e', 's', 't'},
		WallClockSeconds: 120,
		CPUSeconds:       110,
		CreatedAt:        time.Now().UTC(),
	}
	if err := SavePendingResult(dir, pr); err != nil {
		t.Fatalf("SavePendingResult: %v", err)
	}

	got, err := ListPendingResults(dir)
	if err != nil {
		t.Fatalf("ListPendingResults: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].WorkUnitID != pr.WorkUnitID || got[0].LeafID != pr.LeafID || got[0].ServerName != pr.ServerName {
		t.Errorf("metadata mismatch: %+v", got[0])
	}
	if string(got[0].RequestProto) != string(pr.RequestProto) {
		t.Errorf("RequestProto mismatch: %v", got[0].RequestProto)
	}
	if got[0].WallClockSeconds != 120 || got[0].CPUSeconds != 110 {
		t.Errorf("metrics mismatch: wall=%d cpu=%d", got[0].WallClockSeconds, got[0].CPUSeconds)
	}
}

func TestSavePendingResult_OverwritesSameWorkUnit(t *testing.T) {
	dir := t.TempDir()
	base := PendingResult{WorkUnitID: "dc5ff9da-f084-4dd7-86b8-e829669814f8", ServerName: "a", CreatedAt: time.Now()}

	if err := SavePendingResult(dir, base); err != nil {
		t.Fatal(err)
	}
	base.ServerName = "b"
	if err := SavePendingResult(dir, base); err != nil {
		t.Fatal(err)
	}

	got, err := ListPendingResults(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (overwrite, not append)", len(got))
	}
	if got[0].ServerName != "b" {
		t.Errorf("ServerName = %q, want b (latest write)", got[0].ServerName)
	}
}

func TestListPendingResults_OrdersOldestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// Save newest first to prove ordering is by CreatedAt, not write order.
	if err := SavePendingResult(dir, PendingResult{WorkUnitID: "new", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := SavePendingResult(dir, PendingResult{WorkUnitID: "old", CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}

	got, err := ListPendingResults(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].WorkUnitID != "old" || got[1].WorkUnitID != "new" {
		t.Fatalf("order = %v, want [old new]", []string{got[0].WorkUnitID, got[1].WorkUnitID})
	}
}

func TestDeletePendingResult(t *testing.T) {
	dir := t.TempDir()
	if err := SavePendingResult(dir, PendingResult{WorkUnitID: "dc5ff9da-f084-4dd7-86b8-e829669814f8", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := DeletePendingResult(dir, "dc5ff9da-f084-4dd7-86b8-e829669814f8"); err != nil {
		t.Fatalf("DeletePendingResult: %v", err)
	}
	got, err := ListPendingResults(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0 after delete", len(got))
	}
	// Deleting a non-existent entry is a no-op, not an error.
	if err := DeletePendingResult(dir, "dc5ff9da-f084-4dd7-86b8-e829669814f8"); err != nil {
		t.Errorf("delete of absent entry: %v", err)
	}
}

func TestListPendingResults_EmptyDirReturnsNil(t *testing.T) {
	got, err := ListPendingResults(t.TempDir())
	if err != nil {
		t.Fatalf("ListPendingResults on empty: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestSavePendingResult_RejectsUnsafeID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "../escape", `a\b`, "a/b", "x..y"} {
		if err := SavePendingResult(dir, PendingResult{WorkUnitID: id}); err == nil {
			t.Errorf("SavePendingResult(%q) = nil, want error", id)
		}
	}
}
