-- 00008_volunteer_hosts.down.sql
DROP INDEX IF EXISTS public.idx_wuah_active_by_host;
ALTER TABLE public.results
    DROP COLUMN IF EXISTS host_id;
ALTER TABLE public.work_unit_assignment_history
    DROP COLUMN IF EXISTS host_id;
DROP INDEX IF EXISTS public.idx_hosts_volunteer_id;
DROP TABLE IF EXISTS public.hosts;
