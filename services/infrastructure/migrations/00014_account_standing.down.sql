ALTER TABLE public.results
    DROP COLUMN IF EXISTS standing_at_submit;

DROP INDEX IF EXISTS idx_volunteers_standing;

ALTER TABLE public.volunteers
    DROP COLUMN IF EXISTS standing_changed_at,
    DROP COLUMN IF EXISTS standing_reason,
    DROP COLUMN IF EXISTS standing_source,
    DROP COLUMN IF EXISTS benched_until,
    DROP COLUMN IF EXISTS standing;
