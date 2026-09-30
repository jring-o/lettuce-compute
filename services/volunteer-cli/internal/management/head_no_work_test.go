package management

import (
	"reflect"
	"strings"
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
	env.daemon.HeadStatus().SetNoWork(testServerAddr, "test-server", daemon.HeadNoWork{
		Reason: "inflight_cap", Message: "test-server lets this machine hold 10 tasks at a time right now, and this machine holds 10.", LeafID: "leaf-1", At: at,
	})
	nw, ok := head()["no_work"].(map[string]any)
	if !ok {
		t.Fatalf("no_work absent after the head stated a reason: %v", head())
	}
	if nw["reason"] != "inflight_cap" || nw["message"] != "test-server lets this machine hold 10 tasks at a time right now, and this machine holds 10." ||
		nw["at"] != "2026-09-26T14:05:00Z" {
		t.Errorf("no_work = %v", nw)
	}
	if _, ok := nw["leaves"]; ok {
		t.Errorf("a reason about the machine lists leaves: %v", nw)
	}

	env.daemon.HeadStatus().ClearNoWork(testServerAddr)
	if _, ok := head()["no_work"]; ok {
		t.Errorf("no_work kept after the head sent work: %v", head()["no_work"])
	}
}

// A head that has nothing new for this account on several leaves is one line,
// naming them all, with no command in it: the desktop app shows the message as
// it is.
func TestHeadsCarriesOneLineForTheLeavesAHeadHasNothingNewFor(t *testing.T) {
	env := setupTestEnv(t)
	at := time.Date(2026, 9, 26, 14, 5, 0, 0, time.UTC)
	for i, leaf := range []string{"Leaf Two", "Leaf One"} {
		env.daemon.HeadStatus().SetNoWork(testServerAddr, "test-server", daemon.HeadNoWork{
			Reason: "already_contributed", Leaf: leaf, LeafID: strings.ToLower(strings.ReplaceAll(leaf, " ", "-")),
			At: at.Add(time.Duration(i) * time.Minute),
		})
	}
	body := decodeJSON(t, env.doRequest(t, "GET", "/api/v1/heads", ""))
	nw, ok := body["heads"].([]any)[0].(map[string]any)["no_work"].(map[string]any)
	if !ok {
		t.Fatalf("no_work absent: %v", body)
	}
	if nw["reason"] != "already_contributed" || nw["at"] != "2026-09-26T14:06:00Z" {
		t.Errorf("no_work = %v, want already_contributed at the latest answer", nw)
	}
	if got := nw["leaves"]; !reflect.DeepEqual(got, []any{"Leaf One", "Leaf Two"}) {
		t.Errorf("leaves = %v, want [Leaf One Leaf Two]", got)
	}
	msg, _ := nw["message"].(string)
	if !strings.Contains(msg, "every task test-server has ready for Leaf One and Leaf Two") || strings.Contains(msg, "lettuce-volunteer") {
		t.Errorf("message = %q", msg)
	}
	if _, ok := nw["leaf"]; ok {
		t.Errorf("a line covering two leaves names one of them as its leaf: %v", nw)
	}
}
