package types

import "github.com/google/uuid"

// ID wraps uuid.UUID for consistent JSON/SQL handling across the infrastructure layer.
// uuid.UUID already implements json.Marshaler, json.Unmarshaler, sql.Scanner, and driver.Valuer.
type ID = uuid.UUID

// NewID generates a new UUID v4.
func NewID() ID {
	return uuid.New()
}

// ParseID parses a UUID string. Returns error if invalid.
func ParseID(s string) (ID, error) {
	return uuid.Parse(s)
}

// MustParseID parses a UUID string. Panics if invalid (for tests only).
func MustParseID(s string) ID {
	return uuid.MustParse(s)
}

// NilID returns the zero-value UUID (all zeros).
func NilID() ID {
	return uuid.Nil
}

// IsNilID checks if the ID is the zero-value UUID.
func IsNilID(id ID) bool {
	return id == uuid.Nil
}
