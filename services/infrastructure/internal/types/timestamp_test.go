package types

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNow_ReturnsUTC(t *testing.T) {
	now := Now()
	if now.Location() != time.UTC {
		t.Errorf("Now() should return UTC, got %s", now.Location())
	}
}

func TestNow_MicrosecondPrecision(t *testing.T) {
	now := Now()
	if now.Nanosecond()%1000 != 0 {
		t.Errorf("Now() should be truncated to microsecond precision, got nanosecond remainder %d", now.Nanosecond()%1000)
	}
}

func TestFormatTimestamp(t *testing.T) {
	ts := time.Date(2026, 3, 12, 14, 30, 0, 0, time.UTC)
	result := FormatTimestamp(ts)
	expected := "2026-03-12T14:30:00.000000Z"
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestFormatTimestamp_WithMicroseconds(t *testing.T) {
	ts := time.Date(2026, 3, 12, 14, 30, 0, 123456000, time.UTC)
	result := FormatTimestamp(ts)
	expected := "2026-03-12T14:30:00.123456Z"
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestFormatTimestamp_NonUTC(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	ts := time.Date(2026, 3, 12, 9, 30, 0, 0, loc)
	result := FormatTimestamp(ts)
	if !strings.HasSuffix(result, "Z") {
		t.Errorf("FormatTimestamp should produce Z suffix, got %s", result)
	}
	expected := "2026-03-12T14:30:00.000000Z"
	if result != expected {
		t.Errorf("expected %s, got %s", expected, result)
	}
}

func TestParseTimestamp_Valid(t *testing.T) {
	input := "2026-03-12T14:30:00Z"
	ts, err := ParseTimestamp(input)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) returned error: %v", input, err)
	}
	if ts.Year() != 2026 || ts.Month() != 3 || ts.Day() != 12 {
		t.Errorf("unexpected date: %v", ts)
	}
}

func TestParseTimestamp_Invalid(t *testing.T) {
	invalid := []string{
		"",
		"not-a-timestamp",
		"2026-03-12",
		"2026-13-01T00:00:00Z",
	}
	for _, s := range invalid {
		_, err := ParseTimestamp(s)
		if err == nil {
			t.Errorf("ParseTimestamp(%q) should have returned error", s)
		}
	}
}

func TestTimestamp_MarshalJSON(t *testing.T) {
	ts := NewTimestamp(time.Date(2026, 3, 12, 14, 30, 0, 0, time.UTC))
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	expected := `"2026-03-12T14:30:00.000000Z"`
	if string(data) != expected {
		t.Errorf("expected %s, got %s", expected, string(data))
	}
}

func TestTimestamp_MarshalJSON_ZSuffix(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	ts := NewTimestamp(time.Date(2026, 3, 12, 9, 30, 0, 0, loc))
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	result := string(data)
	if !strings.Contains(result, "Z") {
		t.Errorf("Timestamp JSON should contain Z suffix, got %s", result)
	}
}

func TestTimestamp_UnmarshalJSON(t *testing.T) {
	input := `"2026-03-12T14:30:00Z"`
	var ts Timestamp
	err := json.Unmarshal([]byte(input), &ts)
	if err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if ts.Year() != 2026 || ts.Month() != 3 || ts.Day() != 12 {
		t.Errorf("unexpected date: %v", ts.Time)
	}
}

func TestTimestamp_UnmarshalJSON_Invalid(t *testing.T) {
	invalid := []string{
		`"not-a-timestamp"`,
		`123`,
		`""`,
	}
	for _, input := range invalid {
		var ts Timestamp
		err := json.Unmarshal([]byte(input), &ts)
		if err == nil {
			t.Errorf("json.Unmarshal(%s) should have returned error", input)
		}
	}
}

func TestTimestampNow(t *testing.T) {
	ts := TimestampNow()
	if ts.Location() != time.UTC {
		t.Errorf("TimestampNow() should return UTC, got %s", ts.Location())
	}
	if ts.Nanosecond()%1000 != 0 {
		t.Errorf("TimestampNow() should be truncated to microsecond precision, got nanosecond remainder %d", ts.Nanosecond()%1000)
	}
}

func TestNewTimestamp_UTCAndMicrosecondTruncation(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	input := time.Date(2026, 3, 12, 9, 30, 0, 123456789, loc)
	ts := NewTimestamp(input)
	if ts.Location() != time.UTC {
		t.Errorf("NewTimestamp should convert to UTC, got %s", ts.Location())
	}
	if ts.Nanosecond() != 123456000 {
		t.Errorf("NewTimestamp should truncate to microseconds, got nanosecond %d", ts.Nanosecond())
	}
	if ts.Hour() != 14 || ts.Minute() != 30 {
		t.Errorf("NewTimestamp should convert to UTC time, got %02d:%02d", ts.Hour(), ts.Minute())
	}
}

func TestParseTimestamp_RFC3339Nano(t *testing.T) {
	input := "2026-03-12T14:30:00.123456789Z"
	ts, err := ParseTimestamp(input)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) returned error: %v", input, err)
	}
	if ts.Year() != 2026 || ts.Month() != 3 || ts.Day() != 12 {
		t.Errorf("unexpected date: %v", ts)
	}
	if ts.Nanosecond() != 123456789 {
		t.Errorf("expected nanosecond 123456789, got %d", ts.Nanosecond())
	}
}

func TestParseTimestamp_Microseconds(t *testing.T) {
	input := "2026-03-12T14:30:00.123456Z"
	ts, err := ParseTimestamp(input)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) returned error: %v", input, err)
	}
	if ts.Nanosecond() != 123456000 {
		t.Errorf("expected nanosecond 123456000, got %d", ts.Nanosecond())
	}
}

func TestTimestamp_RoundTrip(t *testing.T) {
	original := NewTimestamp(time.Date(2026, 3, 12, 14, 30, 0, 123456000, time.UTC))

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var restored Timestamp
	err = json.Unmarshal(data, &restored)
	if err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	// Microsecond precision is preserved through JSON round-trip.
	if !original.Time.Equal(restored.Time) {
		t.Errorf("round-trip mismatch: %v != %v", original.Time, restored.Time)
	}
}
