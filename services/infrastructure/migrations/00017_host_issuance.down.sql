-- Revert 00017_host_issuance: restore the host_key column and its uniqueness
-- constraint. Issued rows never had a client host key, so backfill each row with its
-- own id text — unique per row by construction, so the restored UNIQUE constraint
-- cannot conflict and the down migration is total.
ALTER TABLE public.hosts
    ADD COLUMN host_key text;

UPDATE public.hosts SET host_key = id::text;

ALTER TABLE public.hosts
    ALTER COLUMN host_key SET NOT NULL;

ALTER TABLE public.hosts
    ADD CONSTRAINT hosts_volunteer_id_host_key_key UNIQUE (volunteer_id, host_key);
