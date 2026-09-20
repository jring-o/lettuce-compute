package daemon

import (
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// tb88OnMachinePlatform puts the test on the Podman-machine path (Windows /
// macOS) whatever the host, so the machine lifecycle is exercised on the
// Linux CI runner too. Kept in its own file so the red half of the TB-88
// regression can be run against the pre-fix tree, where the seam does not
// exist, by swapping this helper for a platform skip.
func tb88OnMachinePlatform(t *testing.T) {
	t.Helper()
	t.Cleanup(runtime.SetNeedsMachineForTest(true))
}
