-- Revert 00016_registration_admission: drop the registration-admission storage.
DROP INDEX IF EXISTS idx_registration_challenges_expiry;
DROP TABLE IF EXISTS public.registration_challenges;
DROP TABLE IF EXISTS public.registration_creation_counts;
