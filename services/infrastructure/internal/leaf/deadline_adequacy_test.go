package leaf

import (
	"encoding/json"
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

// setHeadDefaultForTest sets the head's default deadline for one test and
// restores it afterwards.
func setHeadDefaultForTest(t *testing.T, seconds int) {
	t.Helper()
	orig := headDefaultDeadlineSeconds
	t.Cleanup(func() { headDefaultDeadlineSeconds = orig })
	SetHeadDefaultDeadlineSeconds(seconds)
}

func TestFaultToleranceConfig_ResolveDeadlineSeconds(t *testing.T) {
	tests := []struct {
		name       string
		cfg        FaultToleranceConfig
		want       int
		wantSource string
	}{
		{
			name:       "the leaf's deadline wins",
			cfg:        FaultToleranceConfig{DeadlineSeconds: intPtr(86400)},
			want:       86400,
			wantSource: DeadlineSourceLeaf,
		},
		{
			name:       "no leaf deadline gets the head default",
			cfg:        FaultToleranceConfig{},
			want:       BuiltinDefaultDeadlineSeconds,
			wantSource: DeadlineSourceHeadDefault,
		},
		{
			name:       "a non-positive leaf deadline is no leaf deadline",
			cfg:        FaultToleranceConfig{DeadlineSeconds: intPtr(0)},
			want:       BuiltinDefaultDeadlineSeconds,
			wantSource: DeadlineSourceHeadDefault,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ResolveDeadlineSeconds(); got != tt.want {
				t.Errorf("ResolveDeadlineSeconds() = %d, want %d", got, tt.want)
			}
			if got := tt.cfg.DeadlineSource(); got != tt.wantSource {
				t.Errorf("DeadlineSource() = %q, want %q", got, tt.wantSource)
			}
		})
	}
}

// The head's setting reaches every leaf without a deadline of its own, and no
// leaf that sets one.
func TestSetHeadDefaultDeadlineSeconds_IsLive(t *testing.T) {
	setHeadDefaultForTest(t, 7200)

	if got := (FaultToleranceConfig{}).ResolveDeadlineSeconds(); got != 7200 {
		t.Errorf("no leaf deadline: got %d, want the head's 7200", got)
	}
	if got := (FaultToleranceConfig{DeadlineSeconds: intPtr(900)}).ResolveDeadlineSeconds(); got != 900 {
		t.Errorf("leaf deadline: got %d, want its own 900", got)
	}
	if got := HeadDefaultDeadlineSeconds(); got != 7200 {
		t.Errorf("HeadDefaultDeadlineSeconds() = %d, want 7200", got)
	}

	// A non-positive value leaves the current default in place.
	SetHeadDefaultDeadlineSeconds(0)
	if got := HeadDefaultDeadlineSeconds(); got != 7200 {
		t.Errorf("after SetHeadDefaultDeadlineSeconds(0): got %d, want 7200 kept", got)
	}
}

func TestDeadlineAdequacyWarnings(t *testing.T) {
	tests := []struct {
		name       string
		leaf       *Leaf
		wantWarn   bool
		wantSource string
	}{
		{
			name: "a leaf deadline shorter than max_cpu_seconds warns",
			leaf: &Leaf{
				ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 86400},                // 24h budget
				FaultToleranceConfig: FaultToleranceConfig{DeadlineSeconds: intPtr(10800)}, // 3h deadline
			},
			wantWarn:   true,
			wantSource: "the leaf's deadline_seconds",
		},
		{
			name: "the head default shorter than max_cpu_seconds warns",
			leaf: &Leaf{
				ExecutionConfig: ExecutionConfig{MaxCPUSeconds: 86400},
			},
			wantWarn:   true,
			wantSource: "the head's default",
		},
		{
			name: "deadline at least max_cpu_seconds does not warn",
			leaf: &Leaf{
				ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 7200},
				FaultToleranceConfig: FaultToleranceConfig{DeadlineSeconds: intPtr(18000)},
			},
			wantWarn: false,
		},
		{
			name: "the head default at least max_cpu_seconds does not warn",
			leaf: &Leaf{
				ExecutionConfig: ExecutionConfig{MaxCPUSeconds: 5400},
			},
			wantWarn: false,
		},
		{
			name: "zero max_cpu_seconds is not flagged",
			leaf: &Leaf{
				ExecutionConfig:      ExecutionConfig{MaxCPUSeconds: 0},
				FaultToleranceConfig: FaultToleranceConfig{DeadlineSeconds: intPtr(60)},
			},
			wantWarn: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := DeadlineAdequacyWarnings(tt.leaf)
			if got := len(warnings) > 0; got != tt.wantWarn {
				t.Errorf("DeadlineAdequacyWarnings() warned=%v (%v), want %v", got, warnings, tt.wantWarn)
			}
			// When it warns, the message must name both offending fields and say
			// where the deadline came from, so an operator can act on it.
			if tt.wantWarn && len(warnings) > 0 {
				w := warnings[0]
				if !strings.Contains(w, "max_cpu_seconds") || !strings.Contains(w, "deadline_seconds") {
					t.Errorf("warning should reference max_cpu_seconds and deadline_seconds, got: %q", w)
				}
				if !strings.Contains(w, tt.wantSource) {
					t.Errorf("warning should say the deadline is %s, got: %q", tt.wantSource, w)
				}
			}
		})
	}
}

func TestTranslateRetiredDeadlineKeys(t *testing.T) {
	tests := []struct {
		name string
		// stored is the merged block's deadline_seconds before translation (nil = none).
		stored  *int
		raw     string
		want    *int
		wantErr bool
		// wantNotes is how many notes the caller logs.
		wantNotes int
	}{
		{name: "no retired keys", stored: intPtr(900), raw: `{"deadline_seconds":900}`, want: intPtr(900)},
		{name: "empty block", stored: intPtr(900), raw: ``, want: intPtr(900)},
		{name: "a multiplier sets deadline_seconds", raw: `{"deadline_multiplier":3}`, want: intPtr(10800), wantNotes: 1},
		{name: "a fractional multiplier", raw: `{"deadline_multiplier":0.5}`, want: intPtr(1800), wantNotes: 1},
		{name: "a multiplier replaces a stored deadline", stored: intPtr(900), raw: `{"deadline_multiplier":2}`, want: intPtr(7200), wantNotes: 1},
		{name: "deadline_seconds sent with it wins", stored: intPtr(7200), raw: `{"deadline_multiplier":5,"deadline_seconds":7200}`, want: intPtr(7200), wantNotes: 1},
		{name: "a null deadline_seconds sent with it does not win", raw: `{"deadline_multiplier":2,"deadline_seconds":null}`, want: intPtr(7200), wantNotes: 1},
		{name: "a zero multiplier changes nothing", stored: intPtr(900), raw: `{"deadline_multiplier":0}`, want: intPtr(900), wantNotes: 1},
		{name: "a null multiplier changes nothing", stored: intPtr(900), raw: `{"deadline_multiplier":null}`, want: intPtr(900), wantNotes: 1},
		{name: "no_deadline true clears the leaf deadline", stored: intPtr(900), raw: `{"no_deadline":true}`, want: nil, wantNotes: 1},
		{name: "no_deadline true wins over a multiplier", raw: `{"no_deadline":true,"deadline_multiplier":5}`, want: nil, wantNotes: 2},
		{name: "no_deadline true wins over deadline_seconds sent with it", stored: intPtr(7200), raw: `{"no_deadline":true,"deadline_seconds":7200}`, want: nil, wantNotes: 1},
		{name: "no_deadline false changes nothing", stored: intPtr(900), raw: `{"no_deadline":false}`, want: intPtr(900), wantNotes: 1},
		{name: "no_deadline false with a multiplier", raw: `{"no_deadline":false,"deadline_multiplier":5}`, want: intPtr(18000), wantNotes: 2},
		{name: "a negative multiplier is refused", raw: `{"deadline_multiplier":-1}`, wantErr: true},
		{name: "a multiplier worth under a second is refused", raw: `{"deadline_multiplier":0.0001}`, wantErr: true},
		{name: "a string multiplier is refused", raw: `{"deadline_multiplier":"3"}`, wantErr: true},
		{name: "a string no_deadline is refused", raw: `{"no_deadline":"yes"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := FaultToleranceConfig{DeadlineSeconds: tt.stored}
			notes, apiErr := TranslateRetiredDeadlineKeys(json.RawMessage(tt.raw), &c)
			if tt.wantErr {
				if apiErr == nil {
					t.Fatalf("want an error, got deadline %v and notes %v", c.DeadlineSeconds, notes)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %v", apiErr)
			}
			switch {
			case tt.want == nil && c.DeadlineSeconds != nil:
				t.Errorf("deadline_seconds = %d, want none", *c.DeadlineSeconds)
			case tt.want != nil && (c.DeadlineSeconds == nil || *c.DeadlineSeconds != *tt.want):
				t.Errorf("deadline_seconds = %v, want %d", c.DeadlineSeconds, *tt.want)
			}
			if len(notes) != tt.wantNotes {
				t.Errorf("notes = %q, want %d", notes, tt.wantNotes)
			}
		})
	}
}
