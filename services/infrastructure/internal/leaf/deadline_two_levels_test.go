package leaf

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// A work unit's deadline has two sources: the leaf's deadline_seconds when set,
// else the head's default. These tests build configs from JSON — the shape a
// stored leaf or an API caller supplies — so they state the contract without
// naming Go fields that only one side of the change has.

// headDefaultSixHours is the head's default deadline when the operator has not
// changed it.
const headDefaultSixHours = 21600

func faultToleranceFromJSON(t *testing.T, raw string) FaultToleranceConfig {
	t.Helper()
	var c FaultToleranceConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("decoding fault_tolerance_config %s: %v", raw, err)
	}
	return c
}

// A leaf that says nothing about its deadline gets the head's default — not the
// three hours a defaulted deadline_multiplier used to give it.
func TestDeadline_LeafThatSetsNoneGetsTheHeadDefault(t *testing.T) {
	c := faultToleranceFromJSON(t, `{"max_reassignments":3}`)
	ApplyFaultToleranceConfigDefaults(&c)
	if got := c.ResolveDeadlineSeconds(); got != headDefaultSixHours {
		t.Errorf("a leaf with no deadline resolves to %ds, want the head default %ds", got, headDefaultSixHours)
	}
}

// Stored configs still carry the retired keys (the migration leaves them in place
// for a rollback). They are not a source: only deadline_seconds and the head
// default are.
func TestDeadline_StoredRetiredKeysAreNotASource(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		want   int
	}{
		{"a GREP leaf: no_deadline with a multiplier", `{"no_deadline":true,"deadline_multiplier":3,"max_reassignments":3}`, headDefaultSixHours},
		{"a multiplier with no deadline_seconds", `{"no_deadline":false,"deadline_multiplier":5,"max_reassignments":3}`, headDefaultSixHours},
		{"a backfilled multiplier leaf", `{"no_deadline":false,"deadline_multiplier":5,"deadline_seconds":18000,"max_reassignments":3}`, 18000},
		{"an explicit deadline", `{"no_deadline":false,"deadline_multiplier":3,"deadline_seconds":900,"max_reassignments":3}`, 900},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := faultToleranceFromJSON(t, tt.stored)
			if got := c.ResolveDeadlineSeconds(); got != tt.want {
				t.Errorf("stored %s resolves to %ds, want %ds", tt.stored, got, tt.want)
			}
		})
	}
}

// The six GREP leaves: each unit may use 24 h of CPU, the leaf sets no deadline,
// and every unit is stamped with the head's 6 h. Activation must say so; it used
// to skip the check for exactly these leaves.
func TestDeadlineAdequacy_WarnsWhenTheHeadDefaultIsShorterThanTheCPUBudget(t *testing.T) {
	p := &Leaf{
		ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 86400},
		FaultToleranceConfig: faultToleranceFromJSON(t, `{"no_deadline":true,"deadline_multiplier":3,"max_reassignments":3}`),
	}
	warnings := DeadlineAdequacyWarnings(p)
	if len(warnings) == 0 {
		t.Fatal("no warning for a leaf whose units get the head's 6 h default against a 24 h CPU budget")
	}
	if !strings.Contains(warnings[0], "21600") || !strings.Contains(warnings[0], "86400") {
		t.Errorf("warning should name the 21600 s deadline and the 86400 s budget, got: %q", warnings[0])
	}
}

// updatedFaultTolerance returns the fault_tolerance_config block of an update's
// JSON response, key by key.
func updatedFaultTolerance(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var resp struct {
		FaultToleranceConfig map[string]json.RawMessage `json:"fault_tolerance_config"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decoding update response: %v; body=%s", err, body)
	}
	return resp.FaultToleranceConfig
}

func TestHandleUpdate_RetiredDeadlineKeys(t *testing.T) {
	tests := []struct {
		name string
		// stored is the leaf's fault_tolerance_config before the update ("" = never configured).
		stored string
		body   string
		// wantDeadline is the deadline_seconds the response must carry; 0 = none.
		wantDeadline int
	}{
		{
			name:         "a multiplier becomes deadline_seconds",
			body:         `{"fault_tolerance_config":{"deadline_multiplier":5}}`,
			wantDeadline: 18000,
		},
		{
			name:         "a multiplier replaces a stored deadline_seconds",
			stored:       `{"deadline_seconds":900,"max_reassignments":3}`,
			body:         `{"fault_tolerance_config":{"deadline_multiplier":2}}`,
			wantDeadline: 7200,
		},
		{
			name:         "deadline_seconds sent with a multiplier wins",
			body:         `{"fault_tolerance_config":{"deadline_multiplier":5,"deadline_seconds":7200}}`,
			wantDeadline: 7200,
		},
		{
			name:         "no_deadline true clears the leaf's deadline",
			stored:       `{"deadline_seconds":900,"max_reassignments":3}`,
			body:         `{"fault_tolerance_config":{"no_deadline":true}}`,
			wantDeadline: 0,
		},
		{
			name:         "no_deadline true wins over a multiplier and a deadline sent with it",
			body:         `{"fault_tolerance_config":{"no_deadline":true,"deadline_multiplier":5,"deadline_seconds":7200}}`,
			wantDeadline: 0,
		},
		{
			name:         "a config that sets no deadline stores none",
			body:         `{"fault_tolerance_config":{"max_reassignments":4}}`,
			wantDeadline: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lf := newUpdateTestLeaf()
			if tt.stored != "" {
				lf.FaultToleranceConfig = faultToleranceFromJSON(t, tt.stored)
			}
			h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

			rec := doUpdate(t, h, lf.ID, tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			ftc := updatedFaultTolerance(t, rec.Body.Bytes())
			for _, retired := range []string{"deadline_multiplier", "no_deadline"} {
				if v, ok := ftc[retired]; ok {
					t.Errorf("response still carries the retired %s (%s)", retired, v)
				}
			}
			raw, has := ftc["deadline_seconds"]
			switch {
			case tt.wantDeadline == 0 && has && string(raw) != "null":
				t.Errorf("deadline_seconds = %s, want none (the head's default applies)", raw)
			case tt.wantDeadline != 0 && (!has || string(raw) != jsonInt(tt.wantDeadline)):
				t.Errorf("deadline_seconds = %s, want %d", raw, tt.wantDeadline)
			}
		})
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A non-positive deadline_seconds is refused, and the message points at the head's
// default instead of a "no hard deadline" setting that never existed.
func TestHandleUpdate_NonPositiveDeadlineSecondsNamesTheHeadDefault(t *testing.T) {
	lf := newUpdateTestLeaf()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"fault_tolerance_config":{"deadline_seconds":0}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "no_deadline") || strings.Contains(body, "no hard deadline") {
		t.Errorf("error still offers no_deadline as a way to have no deadline: %s", body)
	}
	if !strings.Contains(body, "default deadline") {
		t.Errorf("error should say to omit deadline_seconds to use the head's default deadline: %s", body)
	}
}

// A negative multiplier is refused, as before.
func TestHandleUpdate_NegativeDeadlineMultiplierRefused(t *testing.T) {
	lf := newUpdateTestLeaf()
	h := &LeafHandler{repo: &mockUpdateRepo{leaf: lf}, logger: slog.Default()}

	rec := doUpdate(t, h, lf.ID, `{"fault_tolerance_config":{"deadline_multiplier":-1}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
