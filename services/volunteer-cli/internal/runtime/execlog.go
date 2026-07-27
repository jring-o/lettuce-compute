package runtime

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExecutionLogName is the file every runtime tees a work unit's stdout+stderr
// into, inside that unit's work dir. It is the only local record of WHY a leaf's
// process failed, and the work dir is removed as soon as the slot finishes, so
// anything that wants to explain a failure must read it before cleanup.
const ExecutionLogName = "execution.log"

// executionLogTailBytes bounds the excerpt attached to a non-zero-exit WARN:
// enough for a stack trace, small enough for one log line.
const executionLogTailBytes = 4096

// noExecutionLog is the placeholder returned when the log is missing, empty or
// unreadable. It is a value a reader can recognize rather than an empty string,
// which would read as "the process said nothing" — a different fact.
const noExecutionLog = "(no captured execution log)"

// ExecutionLogPath returns the execution-log path inside a work dir.
func ExecutionLogPath(workDir string) string {
	return filepath.Join(workDir, ExecutionLogName)
}

// ExecutionLogTail returns up to the last executionLogTailBytes of a work unit's
// execution log. Best-effort: a missing or unreadable log yields noExecutionLog
// rather than an error, because every caller is on a diagnostic path where a
// second failure must not displace the first.
func ExecutionLogTail(workDir string) string {
	if workDir == "" {
		return noExecutionLog
	}
	return tailOfFile(ExecutionLogPath(workDir), executionLogTailBytes)
}

// ExecutionLogSummary renders the execution-log tail as a single line of at most
// maxBytes, for contexts that cannot carry a multi-line blob — above all the
// abandon reason, which crosses the wire to the head and lands in its log.
// Newlines become " / " so the shape of the output survives the flattening.
func ExecutionLogSummary(workDir string, maxBytes int) string {
	tail := ExecutionLogTail(workDir)
	if tail == noExecutionLog {
		return ""
	}
	flat := strings.Join(strings.Fields(strings.ReplaceAll(tail, "\n", " / ")), " ")
	flat = strings.Trim(flat, "/ ")
	if maxBytes > 0 && len(flat) > maxBytes {
		// Keep the TAIL: a failing process's last words explain it, its first
		// words are usually banner output.
		flat = "…" + flat[len(flat)-maxBytes:]
	}
	return flat
}

// tailOfFile returns up to the last maxBytes of the file as a string, or a
// short placeholder when the file is missing/unreadable. Best-effort — used
// only for diagnostics.
func tailOfFile(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return noExecutionLog
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return noExecutionLog
	}
	if st.Size() > maxBytes {
		if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
			return noExecutionLog
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil || len(data) == 0 {
		return noExecutionLog
	}
	return string(data)
}
