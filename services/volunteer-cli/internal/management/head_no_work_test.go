package management

import (
	"testing"
	"time"

	"github.com/lettuce-compute/volunteer-cli/internal/daemon"
)

// GET /api/v1/heads carries the reason a head gave on its latest empty work reply
// that named one, and drops it once the head sends work again. Absent otherwise, so
// a client reads "no reason" from an older daemon and from a head that gave none.
func TestHeadsCarriesTheHeadsStatedNoWorkReason(t *testing.T) {
	env := setupTestEnv(t)
	head := func() map[string]any {
		body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/heads", ""))
		return body["heads"].([]any)[0].(map[string]any)
	}

	if _, ok := head()["no_work"]; ok {
		t.Fatalf("fresh head carries no_work: %v", head()["no_work"])
	}

	at := time.Date(2026, 9, 26, 14, 5, 0, 0, time.UTC)
	env.daemon.HeadStatus().SetNoWork(testServerAddr, daemon.HeadNoWork{
		Reason: "already_contributed", Message: "This account already has a result on every task.", Leaf: "Leaf One", LeafID: "leaf-1", At: at,
	})
	nw, ok := head()["no_work"].(map[string]any)
	if !ok {
		t.Fatalf("no_work absent after the head stated a reason: %v", head())
	}
	if nw["reason"] != "already_contributed" || nw["message"] != "This account already has a result on every task." ||
		nw["leaf"] != "Leaf One" || nw["at"] != "2026-09-26T14:05:00Z" {
		t.Errorf("no_work = %v", nw)
	}

	env.daemon.HeadStatus().ClearNoWork(testServerAddr)
	if _, ok := head()["no_work"]; ok {
		t.Errorf("no_work kept after the head sent work: %v", head()["no_work"])
	}
}
