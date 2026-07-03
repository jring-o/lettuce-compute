package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfirmResolvedDID(t *testing.T) {
	cases := map[string]struct {
		input string
		want  bool
	}{
		"y":            {"y\n", true},
		"yes":          {"yes\n", true},
		"uppercase Y":  {"Y\n", true},
		"YES padded":   {"  YES  \n", true},
		"n":            {"n\n", false},
		"empty (deny)": {"\n", false},
		"no eol":       {"y", true},
		"garbage":      {"maybe\n", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := confirmResolvedDID(strings.NewReader(tc.input), &out, "did:plc:abc123")
			if err != nil {
				t.Fatalf("confirmResolvedDID: %v", err)
			}
			if got != tc.want {
				t.Errorf("confirmResolvedDID(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if !strings.Contains(out.String(), "did:plc:abc123") {
				t.Errorf("prompt should show the DID; got %q", out.String())
			}
		})
	}
}

func TestResolveAppPasswordFlagWins(t *testing.T) {
	t.Setenv(appPasswordEnv, "from-env")
	var warn bytes.Buffer
	pw, err := resolveAppPassword("from-flag", strings.NewReader(""), &warn)
	if err != nil {
		t.Fatalf("resolveAppPassword: %v", err)
	}
	if pw != "from-flag" {
		t.Errorf("password = %q, want from-flag", pw)
	}
	if warn.Len() != 0 {
		t.Errorf("no prompt/warning expected when flag is set, got %q", warn.String())
	}
}

func TestResolveAppPasswordEnvFallback(t *testing.T) {
	t.Setenv(appPasswordEnv, "from-env")
	var warn bytes.Buffer
	pw, err := resolveAppPassword("", strings.NewReader(""), &warn)
	if err != nil {
		t.Fatalf("resolveAppPassword: %v", err)
	}
	if pw != "from-env" {
		t.Errorf("password = %q, want from-env", pw)
	}
	if warn.Len() != 0 {
		t.Errorf("no prompt expected when env is set, got %q", warn.String())
	}
}

func TestResolveAppPasswordPromptWarns(t *testing.T) {
	t.Setenv(appPasswordEnv, "")
	var warn bytes.Buffer
	pw, err := resolveAppPassword("", strings.NewReader("typed-password\n"), &warn)
	if err != nil {
		t.Fatalf("resolveAppPassword: %v", err)
	}
	if pw != "typed-password" {
		t.Errorf("password = %q, want typed-password", pw)
	}
	if !strings.Contains(warn.String(), "not hidden") {
		t.Errorf("prompt should warn that input is not hidden, got %q", warn.String())
	}
}

func TestResolveAppPasswordEmptyPromptErrors(t *testing.T) {
	t.Setenv(appPasswordEnv, "")
	var warn bytes.Buffer
	if _, err := resolveAppPassword("", strings.NewReader("\n"), &warn); err == nil {
		t.Fatal("expected error when no app password is provided")
	}
}
