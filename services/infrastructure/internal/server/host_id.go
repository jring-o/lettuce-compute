package server

import (
	"github.com/google/uuid"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// hostIDNamespace is the fixed UUIDv5 namespace for deriving a stable per-machine host
// id from (account id, host key). It is an arbitrary constant generated once; NEVER
// change it — doing so would re-key every machine's host id and orphan its in-flight
// metering and attribution.
var hostIDNamespace = uuid.MustParse("1688f7b5-44e2-47ba-9b9c-0339bcffdd4a")

// effectiveHostID maps a volunteer (account) id plus the machine's self-reported host
// key to a stable per-MACHINE id. It is the keystone of the account<->host split (TODO
// #19):
//
//   - hostKey == "" (a volunteer that reports no host) -> the account id UNCHANGED, so
//     every per-host map / SQL predicate keyed on the result transparently falls back to
//     today's per-account behavior (host == account). This is the additive, non-breaking
//     path: an old volunteer is never a hard cutover.
//   - hostKey != "" -> a deterministic UUIDv5 of (account id || host key). The same
//     machine always maps to the same host id with NO database lookup (so it is cheap on
//     the RequestWorkUnit hot path), and two machines under one key map to two DISTINCT
//     host ids, each earning its own in-flight budget and work-send clock.
//
// The head stores this same value in hosts.id and in the additive
// work_unit_assignment_history.host_id / results.host_id columns, so COALESCE(host_id,
// volunteer_id) in SQL equals this value (= volunteer_id in the fallback) and the
// in-memory metering and the DB agree with no special-casing.
//
// Per-WU distinctness, credit, RAC, and attestations stay keyed on the ACCOUNT
// (volunteer id) and never call this — a user's own machines must never corroborate the
// same unit, which falls out of key=identity for free.
func effectiveHostID(volunteerID types.ID, hostKey string) types.ID {
	if hostKey == "" {
		return volunteerID
	}
	data := make([]byte, 0, len(volunteerID)+len(hostKey))
	data = append(data, volunteerID[:]...)
	data = append(data, hostKey...)
	return uuid.NewSHA1(hostIDNamespace, data)
}
