package mapreduce

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/lettuce-compute/infrastructure/internal/apierror"
)

// Chunk represents a piece of split data.
type Chunk struct {
	Index    int             // 0-based chunk index
	Data     json.RawMessage // chunk content (for inline data)
	DataRef  *string         // external reference (for external data)
	Metadata map[string]any  // offset, line range, etc.
}

// Splitter splits input data into chunks.
type Splitter interface {
	Split(data []byte, config map[string]any) ([]Chunk, error)
}

// NewSplitter returns the appropriate Splitter for the given strategy name.
func NewSplitter(strategy string) (Splitter, error) {
	switch strategy {
	case "by_line_count":
		return &ByLineCountSplitter{}, nil
	case "by_byte_size":
		return &ByByteSizeSplitter{}, nil
	case "by_record":
		return &ByRecordSplitter{}, nil
	default:
		return nil, apierror.ValidationError(
			fmt.Sprintf("unknown splitting_strategy: %q; must be one of by_line_count, by_byte_size, by_record", strategy),
			nil,
		)
	}
}

const (
	defaultLinesPerChunk   = 1000
	minLinesPerChunk       = 1
	maxLinesPerChunk       = 1_000_000
	defaultBytesPerChunk   = 10_485_760 // 10 MB
	minBytesPerChunk       = 1024       // 1 KB
	maxBytesPerChunk       = 1_073_741_824 // 1 GB
	defaultRecordsPerChunk = 100
	minRecordsPerChunk     = 1
	maxRecordsPerChunk     = 1_000_000
)

// ByLineCountSplitter splits text data by line count.
type ByLineCountSplitter struct{}

func (s *ByLineCountSplitter) Split(data []byte, config map[string]any) ([]Chunk, error) {
	if len(data) == 0 {
		return nil, apierror.ValidationError("input data is empty", nil)
	}

	linesPerChunk := defaultLinesPerChunk
	if v, ok := config["lines_per_chunk"]; ok {
		n, err := toInt(v)
		if err != nil || n < minLinesPerChunk || n > maxLinesPerChunk {
			return nil, apierror.ValidationError(
				fmt.Sprintf("lines_per_chunk must be an integer between %d and %d", minLinesPerChunk, maxLinesPerChunk),
				nil,
			)
		}
		linesPerChunk = n
	}

	// Check that data contains at least one newline (text data).
	if !bytes.Contains(data, []byte("\n")) {
		return nil, apierror.ValidationError("by_line_count requires text data with newlines", nil)
	}

	lines := bytes.Split(data, []byte("\n"))
	// Remove trailing empty element caused by trailing newline.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}

	if len(lines) == 0 {
		return nil, apierror.ValidationError("by_line_count requires text data with newlines", nil)
	}

	var chunks []Chunk
	for i := 0; i < len(lines); i += linesPerChunk {
		end := i + linesPerChunk
		if end > len(lines) {
			end = len(lines)
		}
		chunkLines := lines[i:end]
		chunkData := bytes.Join(chunkLines, []byte("\n"))
		// Add trailing newline.
		chunkData = append(chunkData, '\n')

		chunks = append(chunks, Chunk{
			Index: len(chunks),
			Data:  json.RawMessage(chunkData),
			Metadata: map[string]any{
				"start_line": i,
				"end_line":   end - 1,
				"line_count": end - i,
			},
		})
	}

	return chunks, nil
}

// ByByteSizeSplitter splits data by byte size.
type ByByteSizeSplitter struct{}

func (s *ByByteSizeSplitter) Split(data []byte, config map[string]any) ([]Chunk, error) {
	if len(data) == 0 {
		return nil, apierror.ValidationError("input data is empty", nil)
	}

	bytesPerChunk := defaultBytesPerChunk
	if v, ok := config["bytes_per_chunk"]; ok {
		n, err := toInt(v)
		if err != nil || n < minBytesPerChunk || n > maxBytesPerChunk {
			return nil, apierror.ValidationError(
				fmt.Sprintf("bytes_per_chunk must be an integer between %d and %d", minBytesPerChunk, maxBytesPerChunk),
				nil,
			)
		}
		bytesPerChunk = n
	}

	var chunks []Chunk
	offset := 0
	for offset < len(data) {
		end := offset + bytesPerChunk
		if end >= len(data) {
			// Last chunk: take everything remaining.
			end = len(data)
		} else {
			// Try to find a newline boundary after the threshold.
			newlinePos := bytes.IndexByte(data[end:], '\n')
			if newlinePos >= 0 && newlinePos < bytesPerChunk {
				// Found a newline within 2x threshold — split there (inclusive of newline).
				end = end + newlinePos + 1
			}
			// If no newline found within 2x, split at exact byte boundary.
		}

		chunks = append(chunks, Chunk{
			Index: len(chunks),
			Data:  json.RawMessage(data[offset:end]),
			Metadata: map[string]any{
				"byte_offset": offset,
				"byte_size":   end - offset,
			},
		})
		offset = end
	}

	return chunks, nil
}

// ByRecordSplitter splits structured data by record boundaries.
type ByRecordSplitter struct{}

func (s *ByRecordSplitter) Split(data []byte, config map[string]any) ([]Chunk, error) {
	if len(data) == 0 {
		return nil, apierror.ValidationError("input data is empty", nil)
	}

	delimiter := "\n"
	if v, ok := config["record_delimiter"]; ok {
		d, isStr := v.(string)
		if !isStr || d == "" {
			return nil, apierror.ValidationError("record_delimiter must be a non-empty string", nil)
		}
		delimiter = d
	}

	recordsPerChunk := defaultRecordsPerChunk
	if v, ok := config["records_per_chunk"]; ok {
		n, err := toInt(v)
		if err != nil || n < minRecordsPerChunk || n > maxRecordsPerChunk {
			return nil, apierror.ValidationError(
				fmt.Sprintf("records_per_chunk must be an integer between %d and %d", minRecordsPerChunk, maxRecordsPerChunk),
				nil,
			)
		}
		recordsPerChunk = n
	}

	records := splitByDelimiter(data, []byte(delimiter))

	var chunks []Chunk
	for i := 0; i < len(records); i += recordsPerChunk {
		end := i + recordsPerChunk
		if end > len(records) {
			end = len(records)
		}
		chunkRecords := records[i:end]

		// Wrap records in a JSON array so the chunk is valid JSON for storage.
		var buf bytes.Buffer
		buf.WriteByte('[')
		for j, rec := range chunkRecords {
			if j > 0 {
				buf.WriteByte(',')
			}
			buf.Write(rec)
		}
		buf.WriteByte(']')

		chunks = append(chunks, Chunk{
			Index: len(chunks),
			Data:  json.RawMessage(buf.Bytes()),
			Metadata: map[string]any{
				"start_record": i,
				"end_record":   end - 1,
				"record_count": end - i,
			},
		})
	}

	return chunks, nil
}

// splitByDelimiter splits data by the given delimiter, filtering out empty records.
func splitByDelimiter(data []byte, delimiter []byte) [][]byte {
	parts := bytes.Split(data, delimiter)
	var records [][]byte
	for _, p := range parts {
		if len(p) > 0 {
			records = append(records, p)
		}
	}
	return records
}

// toInt converts a JSON-decoded number to int.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	default:
		return 0, fmt.Errorf("cannot convert %T to int", v)
	}
}
