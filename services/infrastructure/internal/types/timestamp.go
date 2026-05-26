package types

import (
	"fmt"
	"time"
)

// Now returns the current UTC time truncated to microsecond precision,
// matching PostgreSQL TIMESTAMPTZ resolution.
func Now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// timestampFormat is RFC 3339 with fixed microsecond precision, matching PostgreSQL TIMESTAMPTZ.
const timestampFormat = "2006-01-02T15:04:05.000000Z"

// FormatTimestamp formats a time.Time as RFC 3339 with microsecond precision and UTC timezone.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampFormat)
}

// ParseTimestamp parses an RFC 3339 string (with or without fractional seconds) into time.Time.
func ParseTimestamp(s string) (time.Time, error) {
	// Try microsecond format first, then fall back to RFC3339Nano (handles any fractional precision).
	t, err := time.Parse(timestampFormat, s)
	if err == nil {
		return t, nil
	}
	t, err = time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Try plain RFC3339 (no fractional seconds) as last resort.
		return time.Parse(time.RFC3339, s)
	}
	return t, nil
}

// Timestamp is a time.Time that JSON-serializes as an RFC 3339 string with Z suffix.
type Timestamp struct {
	time.Time
}

// NewTimestamp creates a Timestamp from a time.Time, converting to UTC
// and truncating to microsecond precision.
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp{Time: t.UTC().Truncate(time.Microsecond)}
}

// TimestampNow returns the current time as a Timestamp.
func TimestampNow() Timestamp {
	return NewTimestamp(time.Now())
}

// MarshalJSON implements json.Marshaler. Produces RFC 3339 with Z suffix.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", FormatTimestamp(t.Time))), nil
}

// UnmarshalJSON implements json.Unmarshaler. Parses RFC 3339 strings.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return fmt.Errorf("types: timestamp must be a JSON string, got %s", s)
	}
	s = s[1 : len(s)-1]

	parsed, err := ParseTimestamp(s)
	if err != nil {
		return fmt.Errorf("types: invalid timestamp %q: %w", s, err)
	}
	t.Time = parsed.UTC().Truncate(time.Microsecond)
	return nil
}
