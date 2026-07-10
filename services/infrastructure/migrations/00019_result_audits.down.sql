-- Revert 00019_result_audits: drop the audit job table before the registry it references.
-- The down migration is total: both tables are created by the up pair and nothing else
-- references them (verdicts live only here; validation arithmetic never reads them).

DROP INDEX IF EXISTS uq_result_audits_open_unit;
DROP INDEX IF EXISTS idx_result_audits_unit;
DROP INDEX IF EXISTS idx_result_audits_lease;
DROP INDEX IF EXISTS idx_result_audits_claim;
DROP TABLE IF EXISTS result_audits;
DROP TABLE IF EXISTS trusted_runners;
