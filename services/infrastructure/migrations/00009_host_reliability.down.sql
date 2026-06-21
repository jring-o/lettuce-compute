-- 00009_host_reliability.down.sql
-- Reverse 00009: drop the per-host reliability signal table (TODO #54). The index is
-- dropped with the table. Purely additive feature, so removing it just reverts dispatch
-- to the flat per-host in-flight cap.

DROP TABLE IF EXISTS public.host_reliability;
