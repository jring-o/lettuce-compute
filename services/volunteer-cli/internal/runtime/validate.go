package runtime

import (
	"fmt"
	"regexp"
)

// workUnitIDPattern matches a strict canonical UUID (8-4-4-4-12 hex digits,
// either case). Anything else — slashes, backslashes, "..", null bytes, empty
// strings, URN prefixes, brace-wrapped or hyphen-less forms — is rejected.
var workUnitIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateWorkUnitID returns an error unless id is a strict canonical UUID.
//
// SECURITY (H2): the work unit ID is supplied by the head and used as the
// trailing component of on-disk paths (work dirs, result files) and, for
// containers, as a host bind-mount source. A malicious or compromised head
// could otherwise set it to a value like "../../../../etc/cron.d/evil" and
// escape the volunteer's data directory. Restricting it to a canonical UUID
// rejects path separators, parent-directory references, and null bytes,
// closing the traversal primitive. This same validator is reused for any
// head-supplied ID (e.g. leaf IDs) that reaches the filesystem.
func ValidateWorkUnitID(id string) error {
	if id == "" {
		return fmt.Errorf("work unit ID is empty")
	}
	if !workUnitIDPattern.MatchString(id) {
		return fmt.Errorf("invalid work unit ID %q: must be a canonical UUID", id)
	}
	return nil
}
