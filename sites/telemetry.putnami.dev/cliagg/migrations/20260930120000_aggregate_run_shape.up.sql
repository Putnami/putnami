-- Admit the run-shape dimensions the CLI already sends: project and job counts
-- and session duration folded into fixed ranges, and presence-only flags. The
-- caps mirror counterDimensionCaps and contributorDimensionCaps in
-- accumulator.go; an unknown dimension keeps cap 0 and is never stored.
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

CREATE OR REPLACE FUNCTION cli_contributor_dimension_cap(value text) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE value
    WHEN 'command' THEN 10000
    WHEN 'outcome' THEN 10000
    WHEN 'cli_version' THEN 8000
    WHEN 'os' THEN 8000
    WHEN 'arch' THEN 8000
    WHEN 'interactive' THEN 6000
    WHEN 'projects' THEN 6000
    WHEN 'jobs' THEN 6000
    WHEN 'duration' THEN 6000
    WHEN 'flag' THEN 6000
    ELSE 0
END
$$;
