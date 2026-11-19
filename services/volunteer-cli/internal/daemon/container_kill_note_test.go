package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/lettuce-compute/volunteer-cli/internal/runtime"
)

// The note an exit code 137 on a container unit gets names whose memory limit
// the unit was killed at. Before, every kill outside the VM case read "killed,
// usually out of memory at its 7000 MB limit", which volunteers took for their
// own machine running out of memory, on machines allowing Lettuce hundreds of
// gigabytes. The note reaches the head as the abandon reason and the volunteer
// as the leaf-failure record and the leaf-failing notice.

// killNoteHost is a machine with memoryMB allowed for Lettuce and a container
// engine whose VM has vmMB (0: the engine shares the host's memory, as on
// Linux).
func killNoteHost(t *testing.T, memoryMB, vmMB int) *Daemon {
	t.Helper()
	d, _, _ := tb63Daemon(t)
	d.cfg.ResourceLimits.MaxMemoryMB = memoryMB
	d.containerFactory = tb63Factory(t, d, vmMB)
	if !d.RedetectContainerRuntime(context.Background(), false) {
		t.Fatal("RedetectContainerRuntime = false with the engine up")
	}
	return d
}

// killReason runs a container unit declaring declaredMB through the result
// handler as an exit 137 and returns the abandon reason the head is sent and
// the reason the leaf-failure record keeps.
func killReason(t *testing.T, d *Daemon, declaredMB int32, res *runtime.ExecutionResult) (abandon, record string) {
	t.Helper()
	mc := &mockClient{}
	wu := headContainerUnit("wu-killed", "leaf-grep", "ghcr.io/example/grep:1.2", declaredMB)
	d.handleSlotResult(context.Background(), SlotResult{
		WU: wu, Conn: handleSlotResultTestConn(mc),
		Result:         res,
		FailureLogTail: "[lettuce-student] converting paper 3 of 10",
	})
	if mc.lastAbandonReq == nil {
		t.Fatal("no abandon request recorded")
	}
	for _, f := range d.LeafFailureSnapshot() {
		if f.LeafID == "leaf-grep" {
			record = f.LastReason
		}
	}
	return mc.lastAbandonReq.Reason, record
}

func requireAll(t *testing.T, what, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q: %s", what, w, got)
		}
	}
}

func requireNone(t *testing.T, what, got string, banned ...string) {
	t.Helper()
	for _, b := range banned {
		if strings.Contains(got, b) {
			t.Errorf("%s still says %q: %s", what, b, got)
		}
	}
}

// TestContainerKillNote_NamesWhoseLimitItWas: the four machines of the
// original report, plus a leaf that declares no memory and a machine with a VM
// whose memory setting, not the VM, lowered the unit's limit. Every reason
// keeps the "non-zero exit code 137" token that log filters match on.
func TestContainerKillNote_NamesWhoseLimitItWas(t *testing.T) {
	own := []string{"the leaf's own memory limit of 7000 MB", "which the leaf's owner sets", "the same on every machine", "it does not mean this computer ran out of memory"}

	t.Run("Linux, 463,872 MB allowed, no VM", func(t *testing.T) {
		d := killNoteHost(t, 463872, 0)
		abandon, record := killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 137})
		requireAll(t, "abandon reason", abandon, append([]string{"non-zero exit code 137 (killed, most likely", "converting paper 3 of 10"}, own...)...)
		requireAll(t, "leaf-failure record", record, own...)
		requireNone(t, "abandon reason", abandon, "usually out of memory", "463872")
	})

	t.Run("Mac, 7,905 MB allowed, VM of 12,288 MB", func(t *testing.T) {
		d := killNoteHost(t, 7905, 12288)
		abandon, _ := killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 137})
		requireAll(t, "abandon reason", abandon, own...)
		requireNone(t, "abandon reason", abandon, "usually out of memory", "VM")
	})

	t.Run("Linux, allowance lowered to 6,000 MB below the leaf's 7,000", func(t *testing.T) {
		d := killNoteHost(t, 463872, 0)
		lowered := *d.cfg
		lowered.ResourceLimits.MaxMemoryMB = 6000
		d.ApplyConfig(&lowered)
		abandon, record := killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 137})
		both := []string{"memory limit of 6000 MB", "the leaf declares 7000 MB", "this machine's memory setting for Lettuce lowered that to 6000 MB"}
		requireAll(t, "abandon reason", abandon, both...)
		requireAll(t, "leaf-failure record", record, both...)
		requireNone(t, "abandon reason", abandon, "leaf's own", "usually out of memory at its 6000 MB limit")
	})

	t.Run("Mac, 8,192 MB allowed, VM of 2,048 MB: the VM wording, unchanged", func(t *testing.T) {
		d := killNoteHost(t, 8192, 2048)
		abandon, _ := killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 137})
		want := "non-zero exit code 137 (killed for memory: the unit declares 7000 MB but container work on this machine is limited to 1536 MB by the container engine's VM of 2048 MB)"
		if !strings.HasPrefix(abandon, want) {
			t.Errorf("abandon reason = %q, want it to start with the VM wording %q", abandon, want)
		}
	})

	t.Run("a leaf that declares no memory gets Lettuce's default", func(t *testing.T) {
		d := killNoteHost(t, 463872, 0)
		abandon, _ := killReason(t, d, 0, &runtime.ExecutionResult{ExitCode: 137})
		requireAll(t, "abandon reason", abandon, "memory limit of 512 MB", "Lettuce's default for a leaf that declares none", "the same on every machine", "it does not mean this computer ran out of memory")
		requireNone(t, "abandon reason", abandon, "leaf's own")
	})

	t.Run("Mac whose memory setting, not its VM, lowered the limit", func(t *testing.T) {
		d := killNoteHost(t, 6000, 12288) // the VM could hold 11,776 MB
		abandon, _ := killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 137})
		requireAll(t, "abandon reason", abandon, "the leaf declares 7000 MB", "this machine's memory setting for Lettuce lowered that to 6000 MB")
		requireNone(t, "abandon reason", abandon, "VM")
	})

	t.Run("any other exit code is reported as before", func(t *testing.T) {
		d := killNoteHost(t, 463872, 0)
		abandon, _ := killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 2})
		if abandon != "non-zero exit code 2; output: [lettuce-student] converting paper 3 of 10" {
			t.Errorf("exit 2 reason = %q, want the bare exit code and the output", abandon)
		}
	})
}

// TestContainerKillNote_UsesTheLimitTheContainerStartedWith: the volunteer
// lowered the allowance from 463,872 MB to 6,000 MB while a 7,000 MB unit was
// running. The container was created with the 7,000 MB limit and was killed at
// it, so that is the limit the note names, as the leaf's own. Recomputed at the
// kill, it named the 6,000 MB the container never ran under.
func TestContainerKillNote_UsesTheLimitTheContainerStartedWith(t *testing.T) {
	d := killNoteHost(t, 463872, 0)
	lowered := *d.cfg
	lowered.ResourceLimits.MaxMemoryMB = 6000
	d.ApplyConfig(&lowered)

	abandon, record := killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 137, MemoryLimitMB: 7000})
	requireAll(t, "abandon reason", abandon, "the leaf's own memory limit of 7000 MB")
	requireAll(t, "leaf-failure record", record, "the leaf's own memory limit of 7000 MB")
	requireNone(t, "abandon reason", abandon, "6000")

	// And the other way: a unit the runtime started below its declaration (the
	// allowance was already lower at start) is described with both figures,
	// even after the allowance went back up.
	raised := *d.cfg
	raised.ResourceLimits.MaxMemoryMB = 463872
	d.ApplyConfig(&raised)
	abandon, _ = killReason(t, d, 7000, &runtime.ExecutionResult{ExitCode: 137, MemoryLimitMB: 6000})
	requireAll(t, "abandon reason", abandon, "memory limit of 6000 MB", "the leaf declares 7000 MB", "lowered that to 6000 MB")
}
