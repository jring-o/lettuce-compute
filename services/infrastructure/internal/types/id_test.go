package types

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestNewID(t *testing.T) {
	id := NewID()
	if id.Version() != 4 {
		t.Errorf("expected UUID v4, got v%d", id.Version())
	}
	if IsNilID(id) {
		t.Error("NewID() should not return nil UUID")
	}
}

func TestNewID_Unique(t *testing.T) {
	a := NewID()
	b := NewID()
	if a == b {
		t.Error("two NewID() calls should produce different IDs")
	}
}

func TestParseID_Valid(t *testing.T) {
	s := "550e8400-e29b-41d4-a716-446655440000"
	id, err := ParseID(s)
	if err != nil {
		t.Fatalf("ParseID(%q) returned error: %v", s, err)
	}
	if id.String() != s {
		t.Errorf("expected %s, got %s", s, id.String())
	}
}

func TestParseID_Invalid(t *testing.T) {
	invalid := []string{
		"",
		"not-a-uuid",
		"550e8400-e29b-41d4-a716",
		"ZZZZZZZZ-ZZZZ-ZZZZ-ZZZZ-ZZZZZZZZZZZZ",
	}
	for _, s := range invalid {
		_, err := ParseID(s)
		if err == nil {
			t.Errorf("ParseID(%q) should have returned error", s)
		}
	}
}

func TestMustParseID_Valid(t *testing.T) {
	s := "550e8400-e29b-41d4-a716-446655440000"
	id := MustParseID(s)
	if id.String() != s {
		t.Errorf("expected %s, got %s", s, id.String())
	}
}

func TestMustParseID_Invalid_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustParseID with invalid string should panic")
		}
	}()
	MustParseID("not-a-uuid")
}

func TestNilID(t *testing.T) {
	id := NilID()
	if id != uuid.Nil {
		t.Errorf("NilID() should return all-zero UUID, got %s", id.String())
	}
}

func TestIsNilID(t *testing.T) {
	if !IsNilID(NilID()) {
		t.Error("IsNilID(NilID()) should return true")
	}
	if IsNilID(NewID()) {
		t.Error("IsNilID(NewID()) should return false")
	}
}

func TestID_JSON_Marshal(t *testing.T) {
	id := MustParseID("550e8400-e29b-41d4-a716-446655440000")
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("json.Marshal(id) returned error: %v", err)
	}
	expected := `"550e8400-e29b-41d4-a716-446655440000"`
	if string(data) != expected {
		t.Errorf("expected %s, got %s", expected, string(data))
	}
}

func TestID_JSON_Unmarshal(t *testing.T) {
	input := `"550e8400-e29b-41d4-a716-446655440000"`
	var id ID
	err := json.Unmarshal([]byte(input), &id)
	if err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if id.String() != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("expected 550e8400-e29b-41d4-a716-446655440000, got %s", id.String())
	}
}

func TestID_JSON_Unmarshal_Invalid(t *testing.T) {
	invalid := []string{
		`"not-a-uuid"`,
		`"550e8400-e29b-41d4"`,
		`123`,
	}
	for _, input := range invalid {
		var id ID
		err := json.Unmarshal([]byte(input), &id)
		if err == nil {
			t.Errorf("json.Unmarshal(%s) should have returned error", input)
		}
	}
}
