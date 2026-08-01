-- 00030_returned_copy_outcome.down.sql
-- PostgreSQL cannot remove a value from an enum type without recreating it. The value is
-- left in place; a head running pre-00030 code treats a RETURNED row like any other closed
-- copy (outcome IS NOT NULL), so it is inert apart from counting toward the raw row count
-- again — the same one-way-door shape as 00011 (SUPERSEDED).
SELECT 1;
