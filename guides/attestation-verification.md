# Verifying Lettuce credit attestations

This guide explains how to independently verify the signed credit records that a Lettuce
head publishes. It is written to stand on its own: a reader with no prior knowledge of the
project and any Ed25519 signature library can follow it end to end. Nothing here depends on
trusting the head that issued the records — that is the whole point.

## 1. What attestations are

Lettuce is a volunteer distributed-computing platform. A **head** is the coordinating
server an operator runs. A **leaf** is a single computation hosted on that head. The head
breaks a leaf into **work units**, hands them out to **volunteers** (people donating spare
compute), and each volunteer runs a unit and returns a **result**. The head compares the
results submitted for the same unit, decides whether they agree, and credits the volunteers
whose work was accepted.

Every one of those credit decisions is recorded as an **attestation**: a small, signed
statement of the form "the head decided outcome *O* for this work unit and credited amount
*A* to this volunteer." Attestations are append-only and digitally signed by the head, so a
third party can confirm the head really made a given decision without having to trust the
system that reports it. There are three kinds:

- a **grant** — the result was accepted and credit was awarded;
- a **rejection** — the result was not accepted (credit zero); and
- a **revocation** — credit previously granted was later clawed back.

Each attestation is signed with the head's Ed25519 key. Verification means rebuilding the
exact bytes the head signed and checking the signature against the head's published public
key.

## 2. Fetching attestations

### Listing — `GET /api/v1/attestations`

Returns a page of attestations plus the head's public key. Query parameters:

| parameter | meaning |
|---|---|
| `limit` | page size (integer) |
| `cursor` | opaque pagination cursor from the previous page |
| `leaf_id` | restrict to one leaf (UUID) |
| `volunteer_public_key` | restrict to one volunteer (base64url, no padding) |
| `from`, `to` | restrict by attestation timestamp |

The response envelope carries three things beyond the rows:

- `signing_public_key` — the head's Ed25519 public key, base64url-encoded without padding.
  This is the key every signature in the page is checked against.
- `signed_fields_by_schema_version` — a manifest mapping each schema version (and the
  revocation form) to the exact ordered list of field names that are covered by the
  signature. This lets a program discover the signed set in-band instead of hard-coding it.
- `verification_recipe` — a pointer back to this document.

Everything needed to verify a row is in the page: the head's public key travels with the
data, and the manifest names the signed fields.

### Convenience verification — `POST /api/v1/attestations/verify`

A single-purpose endpoint that assembles the signed bytes for you.

Request body (capped at 1 KiB):

```json
{"attestation_id": "33333333-3333-3333-3333-333333333333"}
```

Response (HTTP 200):

```json
{
  "attestation_id": "…",
  "schema_version": 2,
  "kind": "grant",
  "signature_valid": true,
  "signed_fields": ["attestation_timestamp", "context", "…"],
  "canonical_payload": "{\"attestation_timestamp\":\"…\"}",
  "revocations": [
    {"attestation_id": "…", "credit_amount": 0.5, "credit_amount_canonical": "0.500000", "attestation_timestamp": "…"}
  ],
  "signing_public_key": "…"
}
```

- `canonical_payload` is the exact byte string the head signed, returned verbatim.
- `signed_fields` names the fields that string is built from.
- `revocations` (grants only) lists every revocation that references this grant, so a
  consumer can compute the net credit (see §7). It is `[]` when there are none.
- `signature_valid` is a plain boolean. An **invalid** signature is still reported with
  HTTP 200 — the endpoint answers the question honestly either way. An unknown id returns
  404; a malformed body or id returns 400.

**Trust model.** The verify endpoint is a convenience, not the root of trust. Because it
returns `canonical_payload` and `signing_public_key`, you never have to believe its
`signature_valid` flag: run the Ed25519 check yourself against the head's published key. The
**trust-minimized path** goes one step further — rebuild the canonical bytes yourself from
the listed row fields using the rules below, and verify against a public key you pinned
ahead of time. Then you rely on the head for nothing but the raw field values, which are
themselves what the signature commits to.

## 3. The signature scheme

Every attestation is signed with **Ed25519** over a **canonical JSON byte string**. All
binary values (public keys, signatures) are encoded as **base64url without padding**
(RFC 4648 §5, the URL-safe alphabet, trailing `=` stripped). The head holds **one** signing
key; its public half is what `signing_public_key` carries.

The canonical byte string is a JSON object with these properties:

- keys sorted in ascending (byte-wise) order;
- no insignificant whitespace anywhere;
- values encoded exactly as specified per field below.

Verifying a signature is therefore three steps: (1) rebuild the canonical bytes for the
row's rule, (2) base64url-decode the signature and the signing public key, (3) run the
Ed25519 verification. There is no separate hashing step to get wrong — Ed25519 hashes the
message internally.

**Single-key caveat — read this before you build anything durable.** There is no key
rotation mechanism. The head signs everything, forever, with the one key it was given. If an
operator loses or replaces that key, **every previously published attestation stops
verifying** — the history is not re-signed. Two consequences:

- Operators must preserve the signing key for the life of the head. (The head refuses to
  start without its key file rather than silently minting a new identity.)
- Consumers should **pin** the published `signing_public_key` out of band and compare it on
  every fetch, rather than blindly trusting whatever key the current envelope advertises.

## 4. Which rule applies: schema version dispatch

Each row carries a `schema_version` integer that selects the canonical rule:

- **`schema_version` 1 — legacy, verify-only.** The original signed form, frozen so old
  records keep verifying. No head writes new v1 rows.
- **`schema_version` 2 — current.** Every attestation issued today is v2.

Within v2, the row's `validation_outcome` selects the sub-form:

- `AGREED` or `DISAGREED` → the **grant/rejection** rule (§6);
- `REVOKED` → the **revocation** rule (§7).

The three forms are domain-separated by an explicit `context` field inside the payload
(v1 has none, and the two v2 contexts differ), so a signature made for one form can never
validate under another.

## 5. The v1 rule (frozen legacy)

A v1 payload is exactly **six** fields, in this order:

| key | encoding |
|---|---|
| `attestation_timestamp` | UTC, format `2006-01-02T15:04:05.000000Z` (six fractional-second digits, `Z` suffix) |
| `credit_amount` | a JSON **number** |
| `leaf_id` | UUID string |
| `validation_outcome` | `"AGREED"` or `"DISAGREED"` |
| `volunteer_public_key` | the volunteer's Ed25519 key, base64url no padding |
| `work_unit_id` | UUID string |

The signature is Ed25519 over those canonical bytes. **v1 is verify-only and carries real
limitations — do not build new trust on it:**

- **No result binding.** The six fields do not include the result identifier. Two
  attestations that share the same six-tuple therefore share the *same* signature: a
  signature is transplantable between them. v2 fixes this by signing `result_id`.
- **Credit precision.** A credit value with more than six decimal places cannot be
  re-verified from the served data, because storage rounds the amount to six places while
  the original signed bytes carried the full-precision number. Such rows report as invalid.
- **`credit_amount` is a JSON number in a language-specific form.** The head renders it as
  Go's shortest round-trip float form, which prints an integer-valued amount with **no**
  decimal point (for example `1`, not `1.0`). Many JSON libraries render `1.0` by default,
  which produces different bytes and a failed verification. Reproducing v1 bytes reliably
  needs a matching float formatter. This fragility is exactly what v2 removes.
- **Very old rows.** Records written before the head adopted the current signed form do not
  verify at all under this rule; they predate it.

Treat a passing v1 verification as "this legacy record is intact," not as a strong binding.
All new work uses v2.

## 6. The v2 grant / rejection rule

This is the current form for accepted (`AGREED`) and rejected (`DISAGREED`) work units. The
payload is **twelve** keys, in this order:

| key | encoding |
|---|---|
| `attestation_timestamp` | UTC, `2006-01-02T15:04:05.000000Z` (six fractional-second digits) |
| `context` | the exact literal `"lettuce/credit-attestation/v2"` |
| `credit_amount` | a JSON **string** with exactly six fractional digits, e.g. `"1.500000"` |
| `leaf_id` | UUID string |
| `output_checksum` | the result's SHA-256 output checksum, 64 lowercase hex characters, or `""` if the result has none |
| `policy_version` | JSON integer (see the changelog below) |
| `quorum_descriptor` | nested JSON object of eight integer fields (below) |
| `result_id` | UUID string of the specific result being attested |
| `schema_version` | JSON integer `2` |
| `validation_outcome` | `"AGREED"` or `"DISAGREED"` |
| `volunteer_public_key` | volunteer's Ed25519 key, base64url no padding |
| `work_unit_id` | UUID string |

Two encoding points deserve emphasis:

- **`credit_amount` is a string, not a number.** Use the fixed-scale decimal string the API
  serves — every v2 row on the list endpoint (and every entry in the verify endpoint's
  `revocations`) carries it as **`credit_amount_canonical`** — and never re-format the
  numeric credit yourself. The head stores and signs the *same* six-digit decimal string, so
  the stored value can never round differently from the signed bytes. (If your client only
  has the numeric amount, formatting it to exactly six fractional digits reproduces the
  string, because the stored value is already at that scale.)
- **`output_checksum` provenance.** For a result whose full output the head received inline,
  this checksum is head-computed. For a *reference-only* result — where the volunteer
  submitted a pointer to output stored elsewhere — this is the checksum the volunteer
  **claimed** and that validation adjudicated on. The attestation states "the outcome was
  computed over this key," not "the head re-fetched and re-hashed these bytes." A later
  release makes it head-verified for reference results; until then, read it with that
  distinction in mind.

### The quorum descriptor

`quorum_descriptor` is a nested object summarizing the agreement event behind the decision.
Its eight fields are all integers (so the payload contains no floating-point numbers at all)
and appear in alphabetical order. They split into what the head's policy **demanded** and
what the comparison actually **delivered**, counted in **distinct-subject** units — multiple
results from one principal corroborate as one, so these are counts of independent
participants, not raw result counts.

| field | meaning |
|---|---|
| `audit_rate_ppm` | the post-hoc audit sampling rate in effect for this leaf, in parts per million (1,000,000 = every result audited; 0 = auditing off) |
| `group_size` | **delivered** — the number of distinct subjects in the agreeing group |
| `min_quorum` | **demanded** — minimum agreeing subjects the policy required |
| `min_trusted_corroborators` | **demanded** — minimum agreeing subjects at or above the trust floor |
| `pending_size` | **delivered** — total distinct subjects that were compared |
| `target_copies` | **demanded** — how many copies of the unit the policy aimed to dispatch |
| `trust_floor` | **demanded** — the trust threshold a subject must meet to count as trusted |
| `trusted_corroborators` | **delivered** — agreeing subjects that met the trust floor |

Carrying demanded and delivered side by side lets a consumer judge how strong a quorum was
without trusting the head's narrative — the numbers speak for themselves.

**On a rejected (`DISAGREED`) unit, `group_size` is the size of the losing group** — the
largest coherent agreeing clique that nonetheless failed the acceptance gates (for example
two out of five under a strict-majority rule). It is zero only when there was no coherent
group at all. The `validation_outcome` states the consequence; the descriptor states the
arithmetic behind it.

### `policy_version` changelog

`policy_version` records which generation of the head's validation-and-credit semantics
issued the attestation, so a consumer can interpret the descriptor correctly across future
changes.

| version | semantics |
|---|---|
| 1 | Initial versioned semantics: distinct-subject counting, strict-majority acceptance, the eight-field descriptor above. |

Future changes that alter what a quorum means will bump this number and extend the table.

### Why any JSON library reproduces the bytes

Every string in a v2 payload is constrained to a character set that no JSON serializer
escapes: UUIDs, lowercase hex, base64url, the fixed timestamp format, the fixed context
literals, and (for revocations) the reason code set in §7. There are no floating-point
numbers — the credit is a string and the descriptor is all integers. Consequently **any**
JSON writer that emits keys in sorted order with no whitespace produces the identical bytes,
in any language. You do not need to reproduce any one implementation's escaping or
float-formatting quirks; there are none in the payload to reproduce.

## 7. The v2 revocation rule

When previously granted credit is clawed back, the head issues a revocation attestation. Its
payload is **eleven** keys, in this order, under a **distinct context**:

| key | encoding |
|---|---|
| `adjustment_id` | UUID of the ledger adjustment that caused the clawback |
| `attestation_timestamp` | UTC, `2006-01-02T15:04:05.000000Z` |
| `context` | the exact literal `"lettuce/credit-attestation-revocation/v2"` |
| `credit_amount` | the revoked magnitude as a positive six-digit decimal string, e.g. `"0.500000"` |
| `leaf_id` | UUID string |
| `reason` | a machine-readable reason code matching `^[A-Z0-9_]{1,64}$` (e.g. `OPERATOR_CLAWBACK` for manual operator clawbacks; `AUDIT_MISMATCH` and `AUDIT_MISMATCH_UNMATURED` for automated audit enforcement — see the note after Netting) |
| `result_id` | UUID string of the result whose grant is being revoked |
| `revokes_attestation_id` | UUID of the original grant attestation being revoked |
| `schema_version` | JSON integer `2` |
| `volunteer_public_key` | volunteer's Ed25519 key, base64url no padding |
| `work_unit_id` | UUID string |

The distinct context string is what separates "the head attests it granted credit" from "the
head attests it revoked credit"; the revocation form has no `validation_outcome` field of its
own — the context *is* the statement type. There is no `quorum_descriptor` or `policy_version`
(a revocation is not a validation event). The `reason` charset is deliberately narrow so the
signed bytes stay escape-free under any serializer.

### Netting

Credit is revoked by issuing revocation attestations, not by rewriting the original grant.
A single grant may be revoked in parts, producing several revocations that each reference it.
The **effective credit** of a grant is therefore:

```
effective_credit(grant) = grant.credit_amount − Σ revocation.credit_amount
                          for every revocation whose revokes_attestation_id == grant.id
```

The verify endpoint's `revocations` list (§2) returns exactly the referencing revocations for
a grant, so a consumer can compute the net without scanning the whole table.

### Audit enforcement: repair grants and multiple outcome rows per result

Heads can re-execute a sample of validated work on operator-vetted trusted runner machines
and, when two independent runners both refute an accepted output, automatically claw back
the fraudulent credit (revocations with `reason` `AUDIT_MISMATCH`, plus
`AUDIT_MISMATCH_UNMATURED` for the sanctioned accounts' other still-immature grants) and
**repair** honest dissenters: a result that originally lost the vote but matches the
re-executed ground truth is re-adjudicated, credited, and issued a NEW `AGREED` v2
attestation. Two consequences for consumers:

- A single `result_id` may carry more than one outcome row — typically a `DISAGREED`
  (credit 0) row from the original validation followed by a later `AGREED` grant from the
  repair. The chronologically newest outcome row reflects the head's current adjudication
  of that result. Revocations remain the only netting instrument; a credit-0 `DISAGREED`
  row has nothing to net.
- Repair-issued `AGREED` rows verify under the ordinary v2 grant rule (§6) — same canonical
  form, same signed fields. Their `quorum_descriptor` reflects the post-repair agreeing set
  of the unit under the head's current policy numbers; the descriptor field semantics are
  unchanged (`policy_version` does not change for repairs).

## 8. What is *not* signed

The list endpoint serves some fields that are **not** covered by any signature — most notably
`unverified_volunteer_metrics`, the resource figures (CPU, GPU, memory, timings) a volunteer
self-reported and that the head never independently checked. Anything not named in the signed
field set for the row's schema version — that is, anything absent from
`signed_fields_by_schema_version` (or the verify endpoint's `signed_fields`) — is outside the
signature.

The fail direction is safe. If a verifier mistakenly folds an unsigned field into the bytes it
reconstructs, the signature simply fails to verify — you get a verification **failure**, never
false trust in an attacker-influenced value. Build your reconstruction from the signed field
list alone, and treat the rest as informational only.

## 9. Worked example

The values below form a complete, self-checking example. The keys used here are an
**illustrative test keypair generated for this document — they are not any head's real
signing key.** Substitute a real head's `signing_public_key` and its served rows when
verifying production data.

<!-- GOLDEN-VECTOR: illustrative fixture generated for this guide (deterministic Ed25519 seed
     0x000102…1f); the maintainers may replace this whole block with the repository's pinned
     golden-vector test fixture so this document and that test share one source of truth. -->

Illustrative signing public key (base64url, no padding):

```
A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg
```

### 9a. A v2 grant

Canonical bytes the head signed (686 bytes, shown on one line — there is no whitespace in the
real payload):

```
{"attestation_timestamp":"2026-07-10T12:34:56.000000Z","context":"lettuce/credit-attestation/v2","credit_amount":"1.500000","leaf_id":"11111111-1111-1111-1111-111111111111","output_checksum":"3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea","policy_version":1,"quorum_descriptor":{"audit_rate_ppm":1000000,"group_size":3,"min_quorum":3,"min_trusted_corroborators":1,"pending_size":3,"target_copies":5,"trust_floor":100,"trusted_corroborators":2},"result_id":"33333333-3333-3333-3333-333333333333","schema_version":2,"validation_outcome":"AGREED","volunteer_public_key":"Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc","work_unit_id":"22222222-2222-2222-2222-222222222222"}
```

Signature (base64url, no padding):

```
wQ2Dx18WtjksEmKkGZMQCwRLvXrp4xXwUwnXw4FSEVVcXXCvGwibgdMS1BQifxb4BpiJi0Bt9WCZRcQdsq_cCg
```

This fixture is deliberately at the edges: `audit_rate_ppm` is the maximum 1,000,000, and
every string is at its widest allowed form — yet no character in the payload is one any JSON
serializer would escape, which is why the byte string is reproducible anywhere.

### 9b. A v2 revocation

Canonical bytes (525 bytes, one line):

```
{"adjustment_id":"55555555-5555-5555-5555-555555555555","attestation_timestamp":"2026-07-10T13:00:00.000000Z","context":"lettuce/credit-attestation-revocation/v2","credit_amount":"1.500000","leaf_id":"11111111-1111-1111-1111-111111111111","reason":"OPERATOR_CLAWBACK","result_id":"33333333-3333-3333-3333-333333333333","revokes_attestation_id":"44444444-4444-4444-4444-444444444444","schema_version":2,"volunteer_public_key":"Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc","work_unit_id":"22222222-2222-2222-2222-222222222222"}
```

Signature (base64url, no padding):

```
6gzkqfjEjlDi03G2VKKvRdbQqzxhlnuo3pxTFob_Cn49ZASkrvSvfH3b6jfv2srg0t0sDkuEVUyxhF3BRHcBAg
```

### 9c. Verification steps

For either example:

1. Obtain the canonical bytes — either take `canonical_payload` from the verify endpoint, or
   rebuild them yourself from the row using the rule for its schema version (keys sorted, no
   whitespace, encodings per §5–§7).
2. base64url-decode (no padding) the `signature` and the `signing_public_key`.
3. Run Ed25519 verification over the canonical bytes. It returns true only if the bytes,
   signature, and key all match.

### 9d. Reproducing it with Python

Uses only the standard-library `json` module to build the bytes, plus the `cryptography`
package for Ed25519. The stdlib serializer reproduces the head's bytes **because the payload
is escape-free and float-free** (§6) — `sort_keys` gives the key order and `separators` drops
the whitespace; nothing else is needed.

```python
import json, base64
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

def b64u_decode(s: str) -> bytes:
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))

grant = {
    "attestation_timestamp": "2026-07-10T12:34:56.000000Z",
    "context": "lettuce/credit-attestation/v2",
    "credit_amount": "1.500000",              # a STRING — never re-format the number
    "leaf_id": "11111111-1111-1111-1111-111111111111",
    "output_checksum": "3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea",
    "policy_version": 1,
    "quorum_descriptor": {
        "audit_rate_ppm": 1000000, "group_size": 3, "min_quorum": 3,
        "min_trusted_corroborators": 1, "pending_size": 3, "target_copies": 5,
        "trust_floor": 100, "trusted_corroborators": 2,
    },
    "result_id": "33333333-3333-3333-3333-333333333333",
    "schema_version": 2,
    "validation_outcome": "AGREED",
    "volunteer_public_key": "Kay64UG8yvCyLhqU000LxzYeUm0L_hLIl5S8kyKWbdc",
    "work_unit_id": "22222222-2222-2222-2222-222222222222",
}

canonical = json.dumps(grant, sort_keys=True, separators=(",", ":")).encode("utf-8")

pub = Ed25519PublicKey.from_public_bytes(b64u_decode("A6EHv_POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg"))
sig = b64u_decode("wQ2Dx18WtjksEmKkGZMQCwRLvXrp4xXwUwnXw4FSEVVcXXCvGwibgdMS1BQifxb4BpiJi0Bt9WCZRcQdsq_cCg")

pub.verify(sig, canonical)   # raises InvalidSignature on failure; returns None on success
print("verified")
```

Any Ed25519 library works — `PyNaCl`, `libsodium`, `openssl pkeyutl -verify`, Go's
`crypto/ed25519`, and so on. Only the canonical-bytes construction is Lettuce-specific, and
that is just sorted-key, whitespace-free JSON with the field encodings above.

## 10. Operator note: revocation rows and schema rollback

The revocation form and its storage arrived in a schema migration. **Down-migrating past that
migration deletes the revocation rows** (the underlying `credit_adjustments` ledger entries
survive, but the signed revocation attestations do not, and rolling forward again does not
re-emit them). Never down-migrate past it on a head that has issued revocations — you would
permanently drop signed clawback records and cause the attestation list to over-report the
credit that is actually still standing. Ordinary code rollbacks are unaffected; this concerns
schema rollback only.
