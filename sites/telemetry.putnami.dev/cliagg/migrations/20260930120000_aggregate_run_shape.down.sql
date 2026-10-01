-- Remove the run-shape dimensions and every row they admitted, then restore the
-- original six-dimension caps.
DELETE FROM cli_daily_contributor WHERE dimension IN ('projects', 'jobs', 'duration', 'flag');
DELETE FROM cli_daily_counter WHERE dimension IN ('projects', 'jobs', 'duration', 'flag');
DELETE FROM cli_aggregate_overflow WHERE dimension IN ('projects', 'jobs', 'duration', 'flag');
DELETE FROM cli_cardinality_admission WHERE kind IN (
    'counter:projects', 'counter:jobs', 'counter:duration', 'counter:flag',
    'contributor:projects', 'contributor:jobs', 'contributor:duration', 'contributor:flag'
);

CREATE OR REPLACE FUNCTION cli_counter_dimension_cap(value text) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE value
    WHEN 'command' THEN 16
    WHEN 'outcome' THEN 8
    WHEN 'cli_version' THEN 1880
    WHEN 'os' THEN 32
    WHEN 'arch' THEN 32
    WHEN 'interactive' THEN 4
    ELSE 0
END
$$;

CREATE OR REPLACE FUNCTION cli_contributor_dimension_cap(value text) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE value
    WHEN 'command' THEN 10000
    WHEN 'outcome' THEN 10000
    WHEN 'cli_version' THEN 8000
    WHEN 'os' THEN 8000
    WHEN 'arch' THEN 8000
    WHEN 'interactive' THEN 6000
    ELSE 0
END
$$;
