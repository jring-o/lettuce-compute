// Package atproto is the head's minimal client for the AT Protocol (ATProto)
// identity layer. It exists so the head can bind a volunteer's device key to a
// decentralized identifier (DID) that the volunteer controls, and later detect
// when that binding has been revoked or rotated.
//
// Background. In Lettuce, a head hosts computations and volunteers run work
// units for it. A volunteer proves control of a stable online identity by
// holding a local Ed25519 device keypair and publishing a "key authorization"
// record in the repository of their own ATProto account. That record lives on
// the volunteer's Personal Data Server (PDS) and names both the volunteer's DID
// and the device key, signed by the device key itself. A DID resolves — via a
// did:plc directory or a did:web document — to the PDS that hosts the account
// and to the account's ATProto signing key.
//
// What this package does. It resolves a DID to its PDS endpoint and signing key
// (ResolveDID), fetches the key-authorization record from the PDS without
// authentication (GetRecord), verifies that record against the expected DID and
// device key (VerifyKeyAuthorization), and re-checks liveness and key history
// over time (RepoAlive, PLCAuditLog) so the head can notice deletions,
// deactivations, and signing-key rotations. It also encodes and decodes the
// did:key form of an Ed25519 public key (EncodeEd25519DIDKey /
// DecodeEd25519DIDKey).
//
// Where it runs. Identity resolution touches the network and is intended only
// for bind-time verification and slow, scheduled TTL re-checks. It MUST NOT sit
// on the work-dispatch hot path: dispatching work units to volunteers never
// waits on a DID resolution or a PDS round-trip.
//
// Dependencies. The package uses only the Go standard library; the base58btc
// codec required by did:key is implemented here rather than pulled from an
// external dependency.
package atproto
