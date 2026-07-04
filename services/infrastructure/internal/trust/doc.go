// Package trust implements the ACCOUNT-LEVEL trust signal that gates quorum power in
// result validation. Lettuce validates a volunteer's result by redundant corroboration
// (several volunteers run the same work unit; agreeing results validate it). Because a
// volunteer account is a freely-minted Ed25519 keypair, plain copy-counting is Sybil-
// vulnerable: one operator can register many accounts and manufacture "agreement". The
// trust gate closes that hole by requiring a quorum to contain enough DISTINCT, TRUSTED
// SUBJECTS, not merely enough copies.
//
// The guiding principle is "identity must be cheap to HAVE but expensive to have
// TRUSTED": anyone may register and contribute immediately, but their results carry no
// quorum power until the subject has earned trust through corroborated-clean work (or an
// operator has seeded it).
//
// # Distinct from internal/reliability
//
// This package is NOT internal/reliability. Reliability is a per-HOST dispatch shaper
// (TODO #54): it grows or shrinks a machine's in-flight work BUFFER based on observed
// throughput, and it explicitly DISCLAIMS Sybil resistance (a throttled host can mint a
// fresh key; reliability is a fairness/liveness signal, never a ban trigger). Trust is
// the opposite concern: it is a per-ACCOUNT correctness signal that decides whether a
// result may COUNT toward validating a unit. Reliability shapes how much work you get;
// trust shapes whether your answers are believed. They are deliberately separate stores
// with separate keys (host id vs. account subject) and separate math.
//
// # Subjects and the missing foreign key
//
// A subject is the account-level trust key: a bound volunteer's ATProto decentralized
// identifier (DID) when the binding is live, else the per-keypair sentinel
// "vol:<volunteer-uuid>". See SubjectForVolunteer. There is deliberately NO foreign key
// from volunteer_trust.subject (or results.trust_subject) to volunteers: a DID subject
// spans MANY volunteer rows (every device a person runs under one identity), and a
// "vol:<uuid>" subject may outlive the volunteer row it names. A foreign key would be
// semantically wrong, not merely inconvenient — the subject is an identity, not a row.
// This mirrors the same design decision documented on host_reliability.
package trust
