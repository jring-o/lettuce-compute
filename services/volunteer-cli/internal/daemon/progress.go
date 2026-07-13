package daemon

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// ReadProgressFile reads the progress percentage from a work unit's progress
// file. Returns 0 if the file doesn't exist or can't be parsed. The file
// should contain a single number (0-100).
//
// Checks {workDir}/progress.txt (native runtime) and falls back to
// {workDir}/output/progress.txt (container runtime).
func ReadProgressFile(workDir string) float64 {
	candidates := []string{
		filepath.Join(workDir, "progress.txt"),
		filepath.Join(workDir, "output", "progress.txt"),
	}
	for _, path := range candidates {
		// BG-15c: the progress file is leaf-controlled; read it through the shared
		// symlink-safe reader rather than a symlink-following os.ReadFile so a
		// planted symlink is refused (and its target discarded) instead of followed.
		data, err := runtime.ReadRegularNoFollow(path)
		if err != nil {
			continue
		}
		val, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}
		if val < 0 {
			val = 0
		}
		if val > 100 {
			val = 100
		}
		return val
	}
	return 0
}
