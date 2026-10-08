ALTER TABLE cli_aggregate_overflow
    ADD COLUMN dimension text NOT NULL DEFAULT '*';
ALTER TABLE cli_aggregate_overflow DROP CONSTRAINT cli_aggregate_overflow_pkey;
ALTER TABLE cli_aggregate_overflow ADD PRIMARY KEY (day, dimension);

DELETE FROM cli_cardinality_admission WHERE kind IN ('counter', 'contributor');
INSERT INTO cli_cardinality_admission(day, kind, used)
SELECT day, 'counter:' || dimension, count(*)::integer
FROM cli_daily_counter GROUP BY day, dimension
ON CONFLICT (day, kind) DO UPDATE SET used = EXCLUDED.used;
INSERT INTO cli_cardinality_admission(day, kind, used)
SELECT day, 'contributor:' || dimension, count(*)::integer
FROM cli_daily_contributor GROUP BY day, dimension
ON CONFLICT (day, kind) DO UPDATE SET used = EXCLUDED.used;

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

CREATE OR REPLACE FUNCTION cli_bound_device_day() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE admitted integer;
BEGIN
    IF EXISTS (
        SELECT 1 FROM cli_aggregate_overflow
        WHERE day = NEW.day AND dimension IN ('*', 'devices')
    ) THEN
        RETURN NULL;
    END IF;
    IF NEW.day < (now() AT TIME ZONE 'UTC')::date - 34
       OR NEW.day > (now() AT TIME ZONE 'UTC')::date THEN
        RETURN NULL;
    END IF;
    IF EXISTS (SELECT 1 FROM cli_device_day WHERE day = NEW.day AND device_id = NEW.device_id) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('cli-device:' || NEW.day::text, 0));
    IF EXISTS (SELECT 1 FROM cli_device_day WHERE day = NEW.day AND device_id = NEW.device_id) THEN
        RETURN NEW;
    END IF;
    INSERT INTO cli_cardinality_admission(day, kind, used)
    VALUES (NEW.day, 'device', 1)
    ON CONFLICT (day, kind) DO UPDATE
    SET used = cli_cardinality_admission.used + 1
    WHERE cli_cardinality_admission.used < 10000
    RETURNING used INTO admitted;
    IF admitted IS NULL THEN
        INSERT INTO cli_aggregate_overflow(day, dimension)
        VALUES (NEW.day, 'devices') ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION cli_bound_daily_counter() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE admitted integer;
DECLARE cap integer := cli_counter_dimension_cap(NEW.dimension);
BEGIN
    IF EXISTS (
        SELECT 1 FROM cli_aggregate_overflow
        WHERE day = NEW.day AND dimension IN ('*', NEW.dimension)
    ) THEN
        RETURN NULL;
    END IF;
    IF NEW.day < (now() AT TIME ZONE 'UTC')::date - 34
       OR NEW.day > (now() AT TIME ZONE 'UTC')::date OR cap = 0 THEN
        RETURN NULL;
    END IF;
    IF EXISTS (
        SELECT 1 FROM cli_daily_counter
        WHERE day = NEW.day AND dimension = NEW.dimension AND key = NEW.key
    ) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(
        'cli-counter:' || NEW.day::text || ':' || NEW.dimension, 0));
    IF EXISTS (
        SELECT 1 FROM cli_daily_counter
        WHERE day = NEW.day AND dimension = NEW.dimension AND key = NEW.key
    ) THEN
        RETURN NEW;
    END IF;
    INSERT INTO cli_cardinality_admission(day, kind, used)
    VALUES (NEW.day, 'counter:' || NEW.dimension, 1)
    ON CONFLICT (day, kind) DO UPDATE
    SET used = cli_cardinality_admission.used + 1
    WHERE cli_cardinality_admission.used < cap
    RETURNING used INTO admitted;
    IF admitted IS NULL THEN
        INSERT INTO cli_aggregate_overflow(day, dimension)
        VALUES (NEW.day, NEW.dimension) ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION cli_bound_daily_contributor() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE admitted integer;
DECLARE cap integer := cli_contributor_dimension_cap(NEW.dimension);
BEGIN
    IF EXISTS (
        SELECT 1 FROM cli_aggregate_overflow
        WHERE day = NEW.day AND dimension IN ('*', NEW.dimension)
    ) THEN
        RETURN NULL;
    END IF;
    IF NEW.day < (now() AT TIME ZONE 'UTC')::date - 34
       OR NEW.day > (now() AT TIME ZONE 'UTC')::date OR cap = 0 THEN
        RETURN NULL;
    END IF;
    IF EXISTS (
        SELECT 1 FROM cli_daily_contributor
        WHERE day = NEW.day AND dimension = NEW.dimension
          AND key = NEW.key AND device_id = NEW.device_id
    ) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(
        'cli-contributor:' || NEW.day::text || ':' || NEW.dimension, 0));
    IF EXISTS (
        SELECT 1 FROM cli_daily_contributor
        WHERE day = NEW.day AND dimension = NEW.dimension
          AND key = NEW.key AND device_id = NEW.device_id
    ) THEN
        RETURN NEW;
    END IF;
    INSERT INTO cli_cardinality_admission(day, kind, used)
    VALUES (NEW.day, 'contributor:' || NEW.dimension, 1)
    ON CONFLICT (day, kind) DO UPDATE
    SET used = cli_cardinality_admission.used + 1
    WHERE cli_cardinality_admission.used < cap
    RETURNING used INTO admitted;
    IF admitted IS NULL THEN
        INSERT INTO cli_aggregate_overflow(day, dimension)
        VALUES (NEW.day, NEW.dimension) ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;

-- pg_cron can be installed in only one database per cluster. Cloud SQL must set
-- cloudsql.enable_pg_cron=on and cron.database_name to this telemetry database,
-- then an authorized administrator must install pg_cron here and grant the
-- migration role USAGE on cron plus EXECUTE on schedule_in_database.
-- Application migration roles are not cloudsqlsuperuser. Installing the
-- extension in this database proves that cron.database_name targets it; reading
-- that setting directly would require the unnecessarily broad
-- pg_read_all_settings role. Fail hard if the bootstrap has not happened:
-- silently omitting the job would couple expiry to request-serving uptime.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_cron') THEN
        RAISE EXCEPTION
            'pg_cron must be admin-installed in the telemetry database'
            USING ERRCODE = '55000';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_roles
        WHERE rolname = session_user AND rolcanlogin
    ) THEN
        RAISE EXCEPTION
            'pg_cron session user % must be a login role', session_user
            USING ERRCODE = '55000';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION cli_expire_aggregates() RETURNS void
LANGUAGE sql SECURITY DEFINER
SET search_path = public, pg_temp AS $$
WITH
contributors AS (DELETE FROM cli_daily_contributor WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
counters AS (DELETE FROM cli_daily_counter WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
devices AS (DELETE FROM cli_device_day WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
overflows AS (DELETE FROM cli_aggregate_overflow WHERE day < (now() AT TIME ZONE 'UTC')::date - 34 OR day > (now() AT TIME ZONE 'UTC')::date),
flushes AS (DELETE FROM cli_applied_flush WHERE created_at < now() - interval '35 days'),
cron_runs AS (
    DELETE FROM cron.job_run_details
    WHERE jobid IN (
        SELECT jobid FROM cron.job
        WHERE jobname = 'cliagg-retention-' || current_database()
    )
    AND end_time < now() - interval '35 days'
)
DELETE FROM cli_cardinality_admission
WHERE day < (now() AT TIME ZONE 'UTC')::date - 34
   OR day > (now() AT TIME ZONE 'UTC')::date
$$;

-- The managed migration login assumes the non-login database owner role.
-- Return to the authenticated login for scheduling so pg_cron can start its
-- background worker without granting LOGIN to the owner.
SET LOCAL ROLE NONE;

SELECT cron.schedule_in_database(
    'cliagg-retention-' || current_database(),
    '0 0 * * *',
    'SELECT public.cli_expire_aggregates()',
    current_database()
);

SELECT cli_expire_aggregates();
