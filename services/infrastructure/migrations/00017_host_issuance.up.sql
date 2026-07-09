-- 00017_host_issuance.up.sql
-- Server-issued host identity (BG-25, design doc §4.6): the head becomes the sole
-- minter of per-machine host ids. hosts.id changes provenance from the derived
-- UUIDv5(account ‖ client host_key) to a server-generated random UUID returned to the
-- client at registration; the client-generated host_key scheme is retired outright
-- (deliberate alpha hard cutover — compatibility with self-generated keys is a
-- non-goal, recorded in the BG-25 design), so the host_key column and its uniqueness
-- constraint are dropped rather than kept as vestige.
--
-- Everything else about the host model is unchanged: one row per (account, machine),
-- per-machine facts live here, attribution columns and the per-host in-flight metering
-- key on hosts.id, and NOTHING about validation/trust/distinctness keys on hosts
-- (those stay account-level by design — migration 00008's invariant).
--
-- The per-account host cap counts these rows (hard cap on TOTAL rows; rows unseen
-- past the activity window are evicted at mint time), served by the existing
-- idx_hosts_volunteer_id.

ALTER TABLE public.hosts
    DROP CONSTRAINT hosts_volunteer_id_host_key_key;

ALTER TABLE public.hosts
    DROP COLUMN host_key;
