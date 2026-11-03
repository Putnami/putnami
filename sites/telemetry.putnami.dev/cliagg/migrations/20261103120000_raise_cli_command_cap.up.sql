-- Admit a command vocabulary of up to 128 names on one UTC day: the
-- cliusage vocabulary outgrew 64. The caps mirror counterDimensionCaps in
-- accumulator.go; only the command cap changes.
CREATE OR REPLACE FUNCTION cli_counter_dimension_cap(value text) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE value
    WHEN 'command' THEN 128
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
