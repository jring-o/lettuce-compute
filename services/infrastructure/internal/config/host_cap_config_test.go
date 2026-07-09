package config

import "testing"

// TestHostCapDefaults verifies the BG-25 per-account host-cap accessors on a zero-valued
// HeadConfig. The per-account bound is ON BY DEFAULT: an unset (nil) pointer resolves to 10,
// while an EXPLICIT 0 disables the cap and is returned verbatim (never coerced to the
// default). The staleness window resolves to 30 days when unset (0) or negative (both share
// the <= 0 branch), and an explicit positive value is returned verbatim.
func TestHostCapDefaults(t *testing.T) {
	h := HeadConfig{}

	// PerAccount: nil (unset) resolves to the ON-by-default 10.
	if got := h.EffectiveHostCapPerAccount(); got != 10 {
		t.Errorf("EffectiveHostCapPerAccount() = %d, want 10 (default)", got)
	}
	// An explicit 0 disables the cap and is returned verbatim, NOT the default.
	zero := 0
	if got := (HeadConfig{HostCapPerAccount: &zero}).EffectiveHostCapPerAccount(); got != 0 {
		t.Errorf("EffectiveHostCapPerAccount(explicit 0) = %d, want 0 (unlimited, verbatim)", got)
	}
	// An explicit positive value is returned verbatim.
	seven := 7
	if got := (HeadConfig{HostCapPerAccount: &seven}).EffectiveHostCapPerAccount(); got != 7 {
		t.Errorf("EffectiveHostCapPerAccount(7) = %d, want 7 (verbatim)", got)
	}

	// ActiveDays: unset (0) resolves to 30; a negative value shares the same <= 0 branch.
	if got := h.EffectiveHostCapActiveDays(); got != 30 {
		t.Errorf("EffectiveHostCapActiveDays() = %d, want 30 (default)", got)
	}
	if got := (HeadConfig{HostCapActiveDays: -5}).EffectiveHostCapActiveDays(); got != 30 {
		t.Errorf("EffectiveHostCapActiveDays(-5) = %d, want 30 (default)", got)
	}
	if got := (HeadConfig{HostCapActiveDays: 15}).EffectiveHostCapActiveDays(); got != 15 {
		t.Errorf("EffectiveHostCapActiveDays(15) = %d, want 15 (verbatim)", got)
	}
}

// TestHostCapValidate covers the host-cap Validate rules: unset and explicit-in-range knobs
// pass; an explicit 0 per-account (unlimited) and 0 active-days (the unset sentinel) pass;
// and a NEGATIVE per-account bound or active-days window is rejected with an error that names
// the offending field.
func TestHostCapValidate(t *testing.T) {
	neg := -1
	zero := 0
	seven := 7
	tests := []struct {
		name    string
		cfg     HeadConfig
		wantErr bool
		errMsg  string
	}{
		{
			name:    "host cap knobs unset ok",
			cfg:     HeadConfig{Name: "x"},
			wantErr: false,
		},
		{
			name:    "valid explicit host cap knobs",
			cfg:     HeadConfig{Name: "x", HostCapPerAccount: &seven, HostCapActiveDays: 15},
			wantErr: false,
		},
		{
			name:    "explicit zero per-account ok (unlimited)",
			cfg:     HeadConfig{Name: "x", HostCapPerAccount: &zero},
			wantErr: false,
		},
		{
			name:    "zero active days ok (unset sentinel resolves to default)",
			cfg:     HeadConfig{Name: "x", HostCapActiveDays: 0},
			wantErr: false,
		},
		{
			name:    "negative per-account rejected",
			cfg:     HeadConfig{Name: "x", HostCapPerAccount: &neg},
			wantErr: true,
			errMsg:  "host_cap_per_account must be >= 0",
		},
		{
			name:    "negative active days rejected",
			cfg:     HeadConfig{Name: "x", HostCapActiveDays: -1},
			wantErr: true,
			errMsg:  "host_cap_active_days must be >= 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errMsg)
				}
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

// TestHostCapEnvOverrides threads the host-cap env knobs through Load and checks both the raw
// fields and their Effective values.
func TestHostCapEnvOverrides(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, `head: { name: "from-yaml" }`)
	t.Setenv("LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT", "7")
	t.Setenv("LETTUCE_HEAD_HOST_CAP_ACTIVE_DAYS", "15")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Head.HostCapPerAccount == nil || *cfg.Head.HostCapPerAccount != 7 {
		t.Errorf("HostCapPerAccount = %v, want a pointer to 7", cfg.Head.HostCapPerAccount)
	}
	if cfg.Head.HostCapActiveDays != 15 {
		t.Errorf("HostCapActiveDays = %d, want 15", cfg.Head.HostCapActiveDays)
	}
	if got := cfg.Head.EffectiveHostCapPerAccount(); got != 7 {
		t.Errorf("EffectiveHostCapPerAccount() = %d, want 7", got)
	}
	if got := cfg.Head.EffectiveHostCapActiveDays(); got != 15 {
		t.Errorf("EffectiveHostCapActiveDays() = %d, want 15", got)
	}
}

// TestHostCapEnvExplicitZeroDisables pins the pointer semantics of the per-account knob: an
// explicit env "0" must parse to a pointer-to-0 (NOT nil), disabling the cap, rather than
// being indistinguishable from unset (which would resolve to the default 10).
func TestHostCapEnvExplicitZeroDisables(t *testing.T) {
	clearLettuceEnv(t)
	path := writeTestConfig(t, minimalConfig)
	t.Setenv("LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT", "0")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Head.HostCapPerAccount == nil {
		t.Fatal("HostCapPerAccount = nil, want a pointer to 0 (an explicit 0 disables the cap)")
	}
	if *cfg.Head.HostCapPerAccount != 0 {
		t.Errorf("*HostCapPerAccount = %d, want 0", *cfg.Head.HostCapPerAccount)
	}
	if got := cfg.Head.EffectiveHostCapPerAccount(); got != 0 {
		t.Errorf("EffectiveHostCapPerAccount() = %d, want 0 (unlimited, not the default 10)", got)
	}
}

// TestHostCapEnvOverrideInvalid rejects a non-integer per-account cap and a non-integer
// active-days window, and each error names the offending variable.
func TestHostCapEnvOverrideInvalid(t *testing.T) {
	t.Run("non-integer per-account", func(t *testing.T) {
		clearLettuceEnv(t)
		path := writeTestConfig(t, minimalConfig)
		t.Setenv("LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT", "banana")
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for non-integer LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT, got nil")
		}
		if !contains(err.Error(), "LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT") {
			t.Errorf("error = %q, want it to name LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT", err.Error())
		}
	})
	t.Run("non-integer active days", func(t *testing.T) {
		clearLettuceEnv(t)
		path := writeTestConfig(t, minimalConfig)
		t.Setenv("LETTUCE_HEAD_HOST_CAP_ACTIVE_DAYS", "banana")
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error for non-integer LETTUCE_HEAD_HOST_CAP_ACTIVE_DAYS, got nil")
		}
		if !contains(err.Error(), "LETTUCE_HEAD_HOST_CAP_ACTIVE_DAYS") {
			t.Errorf("error = %q, want it to name LETTUCE_HEAD_HOST_CAP_ACTIVE_DAYS", err.Error())
		}
	})
}
