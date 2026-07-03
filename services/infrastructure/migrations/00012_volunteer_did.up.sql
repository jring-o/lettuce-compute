-- 00012_volunteer_did.up.sql
-- Optional ATProto DID identity binding for volunteers.
--
-- A volunteer authenticates to the head with a locally-held Ed25519 keypair
-- (volunteers.public_key, unique). This migration lets a volunteer OPTIONALLY bind
-- that account to a decentralized identifier (DID): the volunteer publishes a
-- key-authorization record in its own ATProto PDS ("Personal Data Server") repo,
-- the head fetches and cryptographically verifies it, and stamps the resulting
-- binding state onto the volunteer row. A background worker re-verifies the binding
-- on a TTL so a later revocation is observed. The binding is advisory identity
-- metadata layered ON TOP of the existing keypair auth; it does not replace or gate
-- it.
--
-- Columns (all nullable except the failure counter, which defaults to 0):
--   did                        the bound decentralized identifier
--   did_binding_uri            AT-URI of the source key-authorization record
--   did_binding_cid            content hash (CID) of that record at last verification
--   did_binding_status         OK | STALE | REVOKED (NULL = not bound)
--   did_bound_at               when the binding was first verified
--   did_binding_checked_at     when the binding was last (re-)verified
--   did_binding_check_failures consecutive failed re-verification attempts
--   did_frozen_until           post-key-rotation re-bind cool-down deadline
--
-- The partial index supports resolving a volunteer by DID and the recheck worker's
-- scan; it is intentionally NOT unique — by design several device rows may share one
-- DID (a person running the fleet under a single identity), so a DID is not an
-- account key.
--
-- ADDITIVE / head-only: every column is nullable (or defaulted) and there is no
-- backfill, so an unbound volunteer's row and behavior are unchanged, existing
-- volunteers keep working against an upgraded head, and the feature stays inert
-- until it is enabled (LETTUCE_HEAD_DID_BINDING_ENABLED). No proto / volunteer-CLI
-- change. A code rollback does not require a schema rollback.

ALTER TABLE public.volunteers
    ADD COLUMN did text,
    ADD COLUMN did_binding_uri text,
    ADD COLUMN did_binding_cid text,
    ADD COLUMN did_binding_status character varying(16)
        CHECK (did_binding_status IN ('OK', 'STALE', 'REVOKED')),
    ADD COLUMN did_bound_at timestamp with time zone,
    ADD COLUMN did_binding_checked_at timestamp with time zone,
    ADD COLUMN did_binding_check_failures integer NOT NULL DEFAULT 0,
    ADD COLUMN did_frozen_until timestamp with time zone;

-- Resolve-by-DID and the recheck worker's scan touch only bound rows, so the index
-- is partial (NULL dids are excluded to keep it small). NOT unique on purpose: many
-- device rows may legitimately share one DID.
CREATE INDEX idx_volunteers_did ON public.volunteers (did) WHERE did IS NOT NULL;
