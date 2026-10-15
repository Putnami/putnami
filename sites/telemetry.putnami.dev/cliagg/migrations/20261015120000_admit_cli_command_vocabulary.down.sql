-- Keep only the eleven catalog commands, recount their admission so no day
-- exceeds the restored cap of 16, then restore that cap.
DELETE FROM cli_daily_contributor
WHERE dimension = 'command' AND key NOT IN (
    'build', 'test', 'lint', 'serve', 'run', 'format',
    'package', 'publish', 'deploy', 'generate', 'validate'
);
DELETE FROM cli_daily_counter
WHERE dimension = 'command' AND key NOT IN (
    'build', 'test', 'lint', 'serve', 'run', 'format',
    'package', 'publish', 'deploy', 'generate', 'validate'
);

UPDATE cli_cardinality_admission AS admission
SET used = (
    SELECT count(*)::integer FROM cli_daily_counter AS counter
    WHERE counter.day = admission.day AND counter.dimension = 'command'
)
WHERE admission.kind = 'counter:command';
UPDATE cli_cardinality_admission AS admission
SET used = (
    SELECT count(*)::integer FROM cli_daily_contributor AS contributor
    WHERE contributor.day = admission.day AND contributor.dimension = 'command'
)
WHERE admission.kind = 'contributor:command';

CREATE OR REPLACE FUNCTION cli_counter_dimension_cap(value text) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE value
    WHEN 'command' THEN 16
    WHEN 'outcome' THEN 8
    WHEN 'cli_version' THEN 1880
    WHEN 'os' THEN 32
    WHEN 'arch' THEN 32
    WHEN 'interactive' THEN 4
    WHEN 'projects' THEN 8
    WHEN 'jobs' THEN 8
    WHEN 'duration' THEN 8
    WHEN 'flag' THEN 8
    ELSE 0
END
$$;
