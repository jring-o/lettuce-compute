package cli

// standardLeafMemoryCapMB is the documented default per-unit memory ceiling for a
// leaf: the head defaults execution_config.max_memory_mb to this and matches a
// volunteer's max_memory_mb against it. A volunteer configured below this matches no
// standard leaf, so init proposes at least this much whenever the machine physically
// has the RAM to back it (#30).
const standardLeafMemoryCapMB = 4096

// proposeMemoryMB derives a default max_memory_mb ceiling from total physical RAM,
// mirroring the CPU numCPU/2 heuristic: roughly half of RAM, floored at the standard
// leaf cap so a freshly-initialized volunteer is eligible for normal leafs — but
// never advertising more memory than the machine physically has (a ceiling it could
// not honor). Falls back to the standard cap when RAM can't be detected.
func proposeMemoryMB(totalMemMB int) int {
	if totalMemMB <= 0 {
		// Detection failed: still cover the standard leaf so onboarding isn't silently
		// starved of work; the operator can lower it if the machine is small.
		return standardLeafMemoryCapMB
	}
	proposed := totalMemMB / 2
	floor := standardLeafMemoryCapMB
	if totalMemMB < floor {
		// A small machine genuinely cannot back the standard cap; advertise what it
		// physically has instead of a ceiling it can't honor.
		floor = totalMemMB
	}
	if proposed < floor {
		proposed = floor
	}
	return proposed
}

// proposeDiskGB derives a default max_disk_gb from free space on the data volume:
// roughly half of free space, floored at the prior static default (10 GB) when the
// volume has at least that much, and capped so a very roomy disk doesn't make the
// fetch gate demand an enormous free margin. Falls back to the static default when
// free space can't be read.
func proposeDiskGB(availDiskMB int64) int {
	const staticDefaultGB = 10
	const maxProposedGB = 50
	if availDiskMB <= 0 {
		return staticDefaultGB
	}
	availGB := int(availDiskMB / 1024)
	if availGB < staticDefaultGB {
		// Don't propose more than the volume can hold.
		return availGB
	}
	proposed := availGB / 2
	if proposed < staticDefaultGB {
		proposed = staticDefaultGB
	}
	if proposed > maxProposedGB {
		proposed = maxProposedGB
	}
	return proposed
}
