-- 00016_registration_admission (additive, head-only): storage for the registration
-- admission gates — the per-IP-per-day creation cap and the registration proof-of-work
-- challenges. Nothing reads either table unless the corresponding
-- LETTUCE_HEAD_REGISTRATION_* knob is enabled (both default off), so this migration is
-- deploy-neutral by construction.
--
-- registration_creation_counts is the durable per-(bucket, UTC day) counter behind the
-- creation cap. The bucket is the canonical string of the client's IPv4 address or IPv6
-- /64 prefix. Rows are incremented inside the same transaction that inserts the new
-- volunteer row, so the cap is exact: a refused or failed registration rolls the
-- increment back. A leader-gated sweeper prunes rows older than a short retention
-- window; the table stays tiny.
CREATE TABLE public.registration_creation_counts (
    bucket        text    NOT NULL,
    day           date    NOT NULL,
    created_count integer NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, day)
);

-- registration_challenges holds server-issued proof-of-work challenges. Unlike
-- identity_challenges (which proves possession of an already-registered key), these are
-- pre-account: public_key deliberately has no foreign key, because the challenge is
-- issued before the volunteer row exists. A challenge is single-use: redemption deletes
-- the row inside the registration transaction. Expired rows are swept periodically.
CREATE TABLE public.registration_challenges (
    id         uuid    NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    public_key bytea   NOT NULL,
    challenge  bytea   NOT NULL,
    difficulty integer NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone NOT NULL DEFAULT now()
);

CREATE INDEX idx_registration_challenges_expiry
    ON public.registration_challenges (expires_at);
