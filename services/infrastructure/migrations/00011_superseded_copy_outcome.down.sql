-- 00011_superseded_copy_outcome.down.sql
-- PostgreSQL cannot remove a value from an enum type without recreating it, and no rows should
-- reference SUPERSEDED on a rollback to a pre-#50 head (it is only written for target>quorum
-- leaves, which do not exist before this feature). The value is left in place; it is inert for
-- any code that does not know about it.
SELECT 1;
