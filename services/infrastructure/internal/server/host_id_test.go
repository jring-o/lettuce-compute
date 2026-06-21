package server

import (
	"testing"

	"github.com/lettuce-compute/infrastructure/internal/types"
)

// effectiveHostID is the keystone of the account<->host split (TODO #19): the no-host
// fallback returns the account id unchanged (so per-host metering folds onto per-account),
// while a reported host key derives a stable, deterministic per-machine id.
func TestEffectiveHostID(t *testing.T) {
	account := types.NewID()
	other := types.NewID()

	// Fallback: an empty host key maps to the account id, so everything keyed on the
	// result transparently behaves per-account (the additive, non-breaking path).
	if got := effectiveHostID(account, ""); got != account {
		t.Errorf("effectiveHostID(account, \"\") = %v, want the account id %v (per-account fallback)", got, account)
	}

	// Deterministic: the same (account, host key) always derives the same id, with no DB
	// lookup — so the hot path can compute it per request.
	h1 := effectiveHostID(account, "machine-laptop")
	h2 := effectiveHostID(account, "machine-laptop")
	if h1 != h2 {
		t.Errorf("effectiveHostID not deterministic: %v vs %v", h1, h2)
	}

	// A derived host id is never the account id, and two machines under one key are
	// distinct (independent metering).
	if h1 == account {
		t.Errorf("derived host id collided with the account id")
	}
	hRig := effectiveHostID(account, "machine-rig")
	if h1 == hRig {
		t.Errorf("two distinct machines under one account derived the same host id")
	}

	// The SAME host key under a DIFFERENT account derives a different id (the account is
	// mixed into the derivation), so one user's host key can't impersonate another's host.
	if effectiveHostID(other, "machine-laptop") == h1 {
		t.Errorf("the same host key under different accounts must derive different host ids")
	}
}

// meterID resolves the per-machine metering key: the host id when present, else the
// account id — equal to effectiveHostID and to SQL's COALESCE(host_id, volunteer_id).
func TestMeterID(t *testing.T) {
	account := types.NewID()
	host := types.NewID()

	if got := meterID(account, nil); got != account {
		t.Errorf("meterID(account, nil) = %v, want %v (fallback to account)", got, account)
	}
	if got := meterID(account, &host); got != host {
		t.Errorf("meterID(account, &host) = %v, want %v", got, host)
	}
}
