package mapreduce

import (
	"strings"
	"testing"
)

// --- ByLineCount tests ---

func TestByLineCount_BasicSplit(t *testing.T) {
	// 10 lines, 3 per chunk → 4 chunks (3,3,3,1)
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = strings.Repeat("x", 10)
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	s := &ByLineCountSplitter{}
	chunks, err := s.Split(data, map[string]any{"lines_per_chunk": float64(3)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(chunks))
	}

	// First 3 chunks have 3 lines, last has 1.
	for i, expected := range []int{3, 3, 3, 1} {
		meta := chunks[i].Metadata
		if meta["line_count"] != expected {
			t.Errorf("chunk %d: expected line_count=%d, got %v", i, expected, meta["line_count"])
		}
	}

	// Verify indices are sequential.
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk %d: expected index=%d, got %d", i, i, c.Index)
		}
	}
}

func TestByLineCount_SingleLine(t *testing.T) {
	data := []byte("single line\n")
	s := &ByLineCountSplitter{}
	chunks, err := s.Split(data, map[string]any{"lines_per_chunk": float64(5)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestByLineCount_EmptyInput(t *testing.T) {
	s := &ByLineCountSplitter{}
	_, err := s.Split([]byte{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestByLineCount_OnlyNewlines(t *testing.T) {
	// \n\n\n is valid text data with 3 empty lines — should produce chunks.
	s := &ByLineCountSplitter{}
	chunks, err := s.Split([]byte("\n\n\n"), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestByLineCount_NoBinaryData(t *testing.T) {
	// Binary data with no newlines should error.
	s := &ByLineCountSplitter{}
	_, err := s.Split([]byte{0x00, 0x01, 0x02, 0xFF}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for binary data with no newlines")
	}
}

func TestByLineCount_DefaultConfig(t *testing.T) {
	// 5 lines with default 1000 → single chunk.
	data := []byte("a\nb\nc\nd\ne\n")
	s := &ByLineCountSplitter{}
	chunks, err := s.Split(data, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestByLineCount_InvalidConfig(t *testing.T) {
	s := &ByLineCountSplitter{}

	tests := []struct {
		name   string
		config map[string]any
	}{
		{"zero", map[string]any{"lines_per_chunk": float64(0)}},
		{"negative", map[string]any{"lines_per_chunk": float64(-1)}},
		{"too large", map[string]any{"lines_per_chunk": float64(2_000_000)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Split([]byte("line\n"), tt.config)
			if err == nil {
				t.Fatal("expected error for invalid config")
			}
		})
	}
}

func TestByLineCount_ExactDivision(t *testing.T) {
	// 6 lines, 3 per chunk → 2 chunks exactly.
	data := []byte("a\nb\nc\nd\ne\nf\n")
	s := &ByLineCountSplitter{}
	chunks, err := s.Split(data, map[string]any{"lines_per_chunk": float64(3)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
}

// --- ByByteSize tests ---

func TestByByteSize_BasicSplit(t *testing.T) {
	// Build data > 2KB, split at 1024 bytes per chunk.
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, strings.Repeat("a", 60)) // ~61 bytes per line with newline
	}
	data := []byte(strings.Join(lines, "\n") + "\n")

	s := &ByByteSizeSplitter{}
	chunks, err := s.Split(data, map[string]any{"bytes_per_chunk": float64(1024)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	// Verify all data is accounted for.
	totalBytes := 0
	for _, c := range chunks {
		totalBytes += len(c.Data)
	}
	if totalBytes != len(data) {
		t.Errorf("expected total bytes %d, got %d", len(data), totalBytes)
	}
}

func TestByByteSize_SmallData(t *testing.T) {
	// Data smaller than one chunk → single chunk.
	data := []byte("small data\n")
	s := &ByByteSizeSplitter{}
	chunks, err := s.Split(data, map[string]any{"bytes_per_chunk": float64(1024)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestByByteSize_EmptyInput(t *testing.T) {
	s := &ByByteSizeSplitter{}
	_, err := s.Split([]byte{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestByByteSize_InvalidConfig(t *testing.T) {
	s := &ByByteSizeSplitter{}
	tests := []struct {
		name   string
		config map[string]any
	}{
		{"zero", map[string]any{"bytes_per_chunk": float64(0)}},
		{"too small", map[string]any{"bytes_per_chunk": float64(100)}},
		{"too large", map[string]any{"bytes_per_chunk": float64(2_000_000_000)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Split([]byte("data\n"), tt.config)
			if err == nil {
				t.Fatal("expected error for invalid config")
			}
		})
	}
}

func TestByByteSize_MetadataFields(t *testing.T) {
	data := []byte(strings.Repeat("x", 2048) + "\n")
	s := &ByByteSizeSplitter{}
	chunks, err := s.Split(data, map[string]any{"bytes_per_chunk": float64(1024)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range chunks {
		if _, ok := c.Metadata["byte_offset"]; !ok {
			t.Error("missing byte_offset in metadata")
		}
		if _, ok := c.Metadata["byte_size"]; !ok {
			t.Error("missing byte_size in metadata")
		}
	}
}

// --- ByRecord tests ---

func TestByRecord_CustomDelimiter(t *testing.T) {
	data := []byte("record1\n---\nrecord2\n---\nrecord3\n---\nrecord4\n---\nrecord5")
	s := &ByRecordSplitter{}
	chunks, err := s.Split(data, map[string]any{
		"record_delimiter":  "\n---\n",
		"records_per_chunk": float64(2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	// First two chunks have 2 records, last has 1.
	for i, expected := range []int{2, 2, 1} {
		if chunks[i].Metadata["record_count"] != expected {
			t.Errorf("chunk %d: expected record_count=%d, got %v", i, expected, chunks[i].Metadata["record_count"])
		}
	}
}

func TestByRecord_DefaultNewlineDelimiter(t *testing.T) {
	data := []byte("line1\nline2\nline3\nline4\nline5\n")
	s := &ByRecordSplitter{}
	chunks, err := s.Split(data, map[string]any{
		"records_per_chunk": float64(2),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
}

func TestByRecord_EmptyInput(t *testing.T) {
	s := &ByRecordSplitter{}
	_, err := s.Split([]byte{}, map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestByRecord_InvalidConfig(t *testing.T) {
	s := &ByRecordSplitter{}

	t.Run("empty delimiter", func(t *testing.T) {
		_, err := s.Split([]byte("data"), map[string]any{"record_delimiter": ""})
		if err == nil {
			t.Fatal("expected error for empty delimiter")
		}
	})

	t.Run("zero records per chunk", func(t *testing.T) {
		_, err := s.Split([]byte("data"), map[string]any{"records_per_chunk": float64(0)})
		if err == nil {
			t.Fatal("expected error for zero records_per_chunk")
		}
	})
}

func TestByRecord_SingleRecord(t *testing.T) {
	data := []byte("single record")
	s := &ByRecordSplitter{}
	chunks, err := s.Split(data, map[string]any{"records_per_chunk": float64(10)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

// --- NewSplitter tests ---

func TestNewSplitter_ValidStrategies(t *testing.T) {
	strategies := []string{"by_line_count", "by_byte_size", "by_record"}
	for _, s := range strategies {
		t.Run(s, func(t *testing.T) {
			splitter, err := NewSplitter(s)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if splitter == nil {
				t.Fatal("expected non-nil splitter")
			}
		})
	}
}

func TestNewSplitter_UnknownStrategy(t *testing.T) {
	_, err := NewSplitter("unknown")
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}
