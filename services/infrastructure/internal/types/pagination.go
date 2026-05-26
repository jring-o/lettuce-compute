package types

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// PaginationRequest represents incoming pagination parameters.
type PaginationRequest struct {
	Cursor   string `json:"cursor"`    // Opaque cursor from previous response. Empty = first page.
	PageSize int    `json:"page_size"` // Requested page size. Clamped to [1, MaxPageSize].
}

// ClampPageSize returns the page size clamped to valid bounds.
// If PageSize is 0 or negative, returns DefaultPageSize.
// If PageSize exceeds MaxPageSize, returns MaxPageSize.
func (p PaginationRequest) ClampPageSize() int {
	if p.PageSize <= 0 {
		return DefaultPageSize
	}
	if p.PageSize > MaxPageSize {
		return MaxPageSize
	}
	return p.PageSize
}

// PaginationResponse represents the pagination metadata in a response.
type PaginationResponse struct {
	NextCursor string `json:"next_cursor"` // Opaque cursor for next page. Empty if no more results.
	HasMore    bool   `json:"has_more"`    // True if there are more results after this page.
}

// EncodeCursor encodes a (created_at, id) pair into an opaque base64url cursor string (no padding).
func EncodeCursor(createdAt time.Time, id ID) string {
	raw := FormatTimestamp(createdAt) + "," + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor decodes an opaque base64url cursor string into (created_at, id).
// Returns error if the cursor is invalid or tampered.
func DecodeCursor(cursor string) (createdAt time.Time, id ID, err error) {
	if cursor == "" {
		return time.Time{}, NilID(), fmt.Errorf("types: cursor is empty")
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, NilID(), fmt.Errorf("types: invalid cursor encoding: %w", err)
	}

	parts := strings.SplitN(string(decoded), ",", 2)
	if len(parts) != 2 {
		return time.Time{}, NilID(), fmt.Errorf("types: invalid cursor format")
	}

	createdAt, err = ParseTimestamp(parts[0])
	if err != nil {
		return time.Time{}, NilID(), fmt.Errorf("types: invalid cursor timestamp: %w", err)
	}

	id, err = ParseID(parts[1])
	if err != nil {
		return time.Time{}, NilID(), fmt.Errorf("types: invalid cursor id: %w", err)
	}

	return createdAt, id, nil
}
