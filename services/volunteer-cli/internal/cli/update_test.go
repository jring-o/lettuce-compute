package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		current string
		latest  string
		want    bool
	}{
		{"0.4.0", "0.6.0", true},
		{"0.6.0", "0.6.0", false},
		{"0.6.0", "0.4.0", false},
		{"1.0.0", "0.9.0", false},
		{"0.9.0", "1.0.0", true},
		{"0.4.0-dev", "0.4.0", true},    // pre-release < release
		{"0.4.0-dev", "0.6.0", true},     // pre-release of older version
		{"0.6.0", "0.6.0-dev", false},    // release > pre-release
		{"0.4.0-dev", "0.4.0-rc1", false}, // both pre-release, same version
		{"0.0.1", "0.0.2", true},
		{"0.0.2", "0.0.1", false},
	}

	for _, tt := range tests {
		got := isNewer(tt.current, tt.latest)
		if got != tt.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"0.4.0", [3]int{0, 4, 0}},
		{"1.2.3", [3]int{1, 2, 3}},
		{"0.6.0-dev", [3]int{0, 6, 0}},
		{"1.0.0-rc1", [3]int{1, 0, 0}},
		{"10.20.30", [3]int{10, 20, 30}},
	}

	for _, tt := range tests {
		got := parseSemver(tt.input)
		if got != tt.want {
			t.Errorf("parseSemver(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("hello world")
	hash := sha256.Sum256(data)
	hashHex := hex.EncodeToString(hash[:])

	t.Run("valid checksum bare", func(t *testing.T) {
		checksumFile := []byte(hashHex + "\n")
		if err := verifyChecksum(data, checksumFile); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("valid checksum with filename", func(t *testing.T) {
		checksumFile := []byte(hashHex + "  lettuce-volunteer-linux-amd64\n")
		if err := verifyChecksum(data, checksumFile); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("invalid checksum", func(t *testing.T) {
		checksumFile := []byte("0000000000000000000000000000000000000000000000000000000000000000\n")
		if err := verifyChecksum(data, checksumFile); err == nil {
			t.Fatal("expected error for mismatched checksum")
		}
	})
}

func TestIsNewer_AlreadyUpToDate(t *testing.T) {
	if isNewer("0.6.0", "0.6.0") {
		t.Error("same version should not be considered newer")
	}
}
