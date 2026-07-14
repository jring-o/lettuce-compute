-- Down for 00026: restore the _cursor key inside splitting_config from the column, then drop the
-- column. The restored value re-establishes the pre-migration storage shape so the older code
-- (which reads data_config -> splitting_config -> '_cursor') resumes at the correct offset.
UPDATE leafs
SET data_config = jsonb_set(
        data_config,
        '{splitting_config,_cursor}',
        generation_cursor
    )
WHERE generation_cursor IS NOT NULL
  AND generation_cursor <> '{}'::jsonb
  AND data_config ? 'splitting_config'
  AND jsonb_typeof(data_config -> 'splitting_config') = 'object';

ALTER TABLE leafs DROP COLUMN generation_cursor;
