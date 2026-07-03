package leaf

import (
	"encoding/json"
	"strings"
	"testing"
)

// allow_external_output is a plain bool stored inside the validation_config JSONB,
// so it needs no migration and no defaults/validation plumbing. These tests pin
// that: it round-trips through JSON, defaults to the secure "false", is not enabled
// by ApplyValidationConfigDefaults, and is accepted by ValidateValidationConfig
// either way.

func TestValidationConfig_AllowExternalOutput_RoundTrip(t *testing.T) {
	cfg := ValidationConfig{
		RedundancyFactor:    2,
		AgreementThreshold:  1.0,
		ComparisonMode:      ComparisonExact,
		MaxRetries:          3,
		AllowExternalOutput: true,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ValidationConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.AllowExternalOutput {
		t.Errorf("AllowExternalOutput did not survive JSON round-trip: %s", data)
	}
}

func TestValidationConfig_AllowExternalOutput_DefaultsFalse(t *testing.T) {
	var cfg ValidationConfig
	if err := json.Unmarshal([]byte(`{"redundancy_factor":1}`), &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.AllowExternalOutput {
		t.Fatal("AllowExternalOutput should default to false when omitted from JSON")
	}
	ApplyValidationConfigDefaults(&cfg)
	if cfg.AllowExternalOutput {
		t.Error("ApplyValidationConfigDefaults must not enable AllowExternalOutput")
	}
}

func TestValidationConfig_AllowExternalOutput_ValidatesEitherWay(t *testing.T) {
	base := func() *ValidationConfig {
		return &ValidationConfig{
			RedundancyFactor:   1,
			AgreementThreshold: 1.0,
			ComparisonMode:     ComparisonExact,
			MaxRetries:         3,
		}
	}
	for _, allow := range []bool{false, true} {
		c := base()
		c.AllowExternalOutput = allow
		if apiErr := ValidateValidationConfig(c); apiErr != nil {
			t.Errorf("ValidateValidationConfig(allow=%v) unexpected error: %v", allow, apiErr)
		}
	}
}

func TestValidationConfig_AllowExternalOutput_OmittedWhenFalse(t *testing.T) {
	// omitempty keeps persisted validation_config JSON clean for the common
	// (disabled) case.
	data, err := json.Marshal(ValidationConfig{RedundancyFactor: 1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "allow_external_output") {
		t.Errorf("a disabled AllowExternalOutput should be omitted from JSON, got: %s", data)
	}
}
