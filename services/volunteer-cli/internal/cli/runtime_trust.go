package cli

import (
	"bufio"
	"fmt"
	"sort"
	"strings"

	rtdetect "github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// containerBackendAvailable reports whether a working container backend (Docker or Podman,
// including a bundled Podman) is present on this machine. Used to decide whether to OFFER
// CONTAINER in the attach consent prompt — trust cannot conjure a capability the machine lacks.
func containerBackendAvailable() bool {
	return detectContainerBackendFunc(rtdetect.BundledPodmanPath()).Backend != rtdetect.BackendNone
}

// parseTrustRuntimes parses a comma-separated runtime list (e.g. "container,native") into the
// UPPERCASE opt-in set stored in config.ServerConfig.TrustedRuntimes. WASM is always trusted, so
// it is accepted but dropped (never stored); "none" (or empty) yields the WASM-only set. Unknown
// names are an error rather than silently ignored. The result is always NON-NIL: an explicit
// "none" must persist as an empty list, not as an absent key that the legacy-trust migration
// would re-seed from available_runtimes (PB-28).
func parseTrustRuntimes(csv string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, part := range strings.Split(csv, ",") {
		p := strings.ToUpper(strings.TrimSpace(part))
		switch p {
		case "", "NONE", "WASM":
			// implicit / no opt-in
		case "CONTAINER", "NATIVE":
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		default:
			return nil, fmt.Errorf("unknown runtime %q (valid: container, native; wasm is always allowed)", strings.TrimSpace(part))
		}
	}
	sort.Strings(out)
	return out, nil
}

// promptRuntimeTrust interactively asks the volunteer how far to trust a head to run code on this
// machine, returning the chosen opt-in runtimes (UPPERCASE; WASM is implicit and never returned).
// CONTAINER is offered only when a backend is present. NATIVE always defaults to no, with an
// explicit warning. On EOF (a non-interactive stream), the prompts take their defaults, so the
// result is the safe posture: WASM plus container-if-available, never native. Always NON-NIL —
// declining everything is an explicit choice that must persist as such (PB-28).
func promptRuntimeTrust(scanner *bufio.Scanner, headName string, containerAvailable bool) []string {
	trusted := []string{}
	fmt.Printf("\nA head is a trust domain — attaching to %q means trusting its operator to run\n", headName)
	fmt.Println("code on this machine. Choose what this head may run. WASM is always allowed: it is")
	fmt.Println("fully sandboxed and cannot touch anything outside its own work folder.")

	if containerAvailable {
		if promptYesNo(scanner, "  Allow CONTAINER tasks from this head? (isolated; uses Docker/Podman)", true) {
			trusted = append(trusted, "CONTAINER")
		}
	} else {
		fmt.Println("  CONTAINER: no Docker/Podman backend detected on this machine — not offered.")
	}

	fmt.Println("  NATIVE runs a program DIRECTLY on this machine with NO sandbox. It can read your")
	fmt.Println("  files — including your identity key — and use your network. Enable it ONLY for an")
	fmt.Println("  operator you fully trust.")
	if promptYesNo(scanner, "  Allow NATIVE tasks from this head?", false) {
		trusted = append(trusted, "NATIVE")
	}
	return trusted
}

// promptYesNo asks a yes/no question with a default and returns the boolean. On EOF or blank
// input it returns the default, so a non-interactive run lands on the safe defaults.
func promptYesNo(scanner *bufio.Scanner, prompt string, def bool) bool {
	suffix := " [y/N]"
	if def {
		suffix = " [Y/n]"
	}
	fmt.Print(prompt + suffix + " ")
	if !scanner.Scan() {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}

// trustSummary renders the runtimes a head is trusted to run for display (always WASM first),
// e.g. "WASM, CONTAINER". An empty opt-in list renders as "WASM".
func trustSummary(trusted []string) string {
	all := []string{"WASM"}
	for _, r := range trusted {
		u := strings.ToUpper(strings.TrimSpace(r))
		if u != "" && u != "WASM" {
			all = append(all, u)
		}
	}
	return strings.Join(all, ", ")
}
