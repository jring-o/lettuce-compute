package types

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClampPageSize_Zero(t *testing.T) {
	p := PaginationRequest{PageSize: 0}
	if got := p.ClampPageSize(); got != DefaultPageSize {
		t.Errorf("ClampPageSize(0) = %d, want %d", got, DefaultPageSize)
	}
}

func TestClampPageSize_Negative(t *testing.T) {
	p := PaginationRequest{PageSize: -10}
	if got := p.ClampPageSize(); got != DefaultPageSize {
		t.Errorf("ClampPageSize(-10) = %d, want %d", got, DefaultPageSize)
	}
}

func TestClampPageSize_ExceedsMax(t *testing.T) {
	p := PaginationRequest{PageSize: 500}
	if got := p.ClampPageSize(); got != MaxPageSize {
		t.Errorf("ClampPageSize(500) = %d, want %d", got, MaxPageSize)
	}
}

func TestClampPageSize_WithinBounds(t *testing.T) {
	tests := []struct {
		size int
		want int
	}{
		{1, 1},
		{50, 50},
		{100, 100},
		{200, 200},
	}
	for _, tt := range tests {
		p := PaginationRequest{PageSize: tt.size}
		if got := p.ClampPageSize(); got != tt.want {
			t.Errorf("ClampPageSize(%d) = %d, want %d", tt.size, got, tt.want)
		}
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	createdAt := time.Date(2026, 3, 12, 14, 30, 0, 0, time.UTC)
	id := MustParseID("550e8400-e29b-41d4-a716-446655440000")

	cursor := EncodeCursor(createdAt, id)
	if cursor == "" {
		t.Fatal("EncodeCursor returned empty string")
	}

	gotTime, gotID, err := DecodeCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeCursor returned error: %v", err)
	}
	if !gotTime.Equal(createdAt) {
		t.Errorf("time mismatch: got %v, want %v", gotTime, createdAt)
	}
	if gotID != id {
		t.Errorf("id mismatch: got %s, want %s", gotID, id)
	}
}

func TestDecodeCursor_Empty(t *testing.T) {
	_, _, err := DecodeCursor("")
	if err == nil {
		t.Error("DecodeCursor(\"\") should return error")
	}
}

func TestDecodeCursor_InvalidBase64(t *testing.T) {
	_, _, err := DecodeCursor("!!!not-base64!!!")
	if err == nil {
		t.Error("DecodeCursor with invalid base64 should return error")
	}
}

func TestDecodeCursor_TamperedContent(t *testing.T) {
	tests := []string{
		"bm8tY29tbWE", // "no-comma" in base64url
		"anVzdCxub3QtYS11dWlk", // "just,not-a-uuid" in base64url
		"bm90LWEtdGltZSw1NTBlODQwMC1lMjliLTQxZDQtYTcxNi00NDY2NTU0NDAwMDA", // "not-a-time,550e8400-..."
	}
	for _, cursor := range tests {
		_, _, err := DecodeCursor(cursor)
		if err == nil {
			t.Errorf("DecodeCursor(%q) should return error for tampered content", cursor)
		}
	}
}

func TestListResponse_JSON_Marshal(t *testing.T) {
	type Item struct {
		Name string `json:"name"`
	}
	resp := NewListResponse([]Item{{Name: "test"}}, PaginationResponse{
		NextCursor: "abc",
		HasMore:    true,
	})

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}

	if _, ok := raw["data"]; !ok {
		t.Error("response missing 'data' field")
	}
	if _, ok := raw["pagination"]; !ok {
		t.Error("response missing 'pagination' field")
	}
}

func TestListResponse_EmptyData_MarshalAsArray(t *testing.T) {
	type Item struct {
		Name string `json:"name"`
	}

	// Test with nil slice
	resp := NewListResponse[Item](nil, PaginationResponse{})
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	result := string(data)
	expected := `"data":[]`
	if !strings.Contains(result, expected) {
		t.Errorf("empty data should marshal as [], got %s", result)
	}
	if strings.Contains(result, `"data":null`) {
		t.Errorf("empty data should not marshal as null, got %s", result)
	}
}

func TestListResponse_EmptySlice_MarshalAsArray(t *testing.T) {
	type Item struct {
		Name string `json:"name"`
	}

	resp := NewListResponse([]Item{}, PaginationResponse{HasMore: false})
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	result := string(data)
	if strings.Contains(result, `"data":null`) {
		t.Errorf("empty slice should marshal as [], got %s", result)
	}
}
