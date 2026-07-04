ALTER TABLE public.volunteers
    DROP COLUMN IF EXISTS rejection_updated_at,
    DROP COLUMN IF EXISTS rejection_bad,
    DROP COLUMN IF EXISTS rejection_good;
