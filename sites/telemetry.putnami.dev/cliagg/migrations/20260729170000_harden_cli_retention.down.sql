SELECT cron.unschedule('cliagg-retention-' || current_database())
WHERE EXISTS (
    SELECT 1 FROM cron.job
    WHERE jobname = 'cliagg-retention-' || current_database()
);
DROP FUNCTION IF EXISTS cli_expire_aggregates();

CREATE OR REPLACE FUNCTION cli_bound_device_day() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE admitted integer;
BEGIN
    IF EXISTS (SELECT 1 FROM cli_aggregate_overflow WHERE day = NEW.day) THEN
        RETURN NULL;
    END IF;
    IF NEW.day < CURRENT_DATE - 35 OR NEW.day > CURRENT_DATE + 1 THEN
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
        INSERT INTO cli_aggregate_overflow(day)
        VALUES (NEW.day) ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION cli_bound_daily_counter() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE admitted integer;
BEGIN
    IF EXISTS (SELECT 1 FROM cli_aggregate_overflow WHERE day = NEW.day) THEN
        RETURN NULL;
    END IF;
    IF NEW.day < CURRENT_DATE - 35 OR NEW.day > CURRENT_DATE + 1 THEN
        RETURN NULL;
    END IF;
    IF EXISTS (
        SELECT 1 FROM cli_daily_counter
        WHERE day = NEW.day AND dimension = NEW.dimension AND key = NEW.key
    ) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('cli-counter:' || NEW.day::text, 0));
    IF EXISTS (
        SELECT 1 FROM cli_daily_counter
        WHERE day = NEW.day AND dimension = NEW.dimension AND key = NEW.key
    ) THEN
        RETURN NEW;
    END IF;
    INSERT INTO cli_cardinality_admission(day, kind, used)
    VALUES (NEW.day, 'counter', 1)
    ON CONFLICT (day, kind) DO UPDATE
    SET used = cli_cardinality_admission.used + 1
    WHERE cli_cardinality_admission.used < 2000
    RETURNING used INTO admitted;
    IF admitted IS NULL THEN
        INSERT INTO cli_aggregate_overflow(day)
        VALUES (NEW.day) ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION cli_bound_daily_contributor() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE admitted integer;
BEGIN
    IF EXISTS (SELECT 1 FROM cli_aggregate_overflow WHERE day = NEW.day) THEN
        RETURN NULL;
    END IF;
    IF NEW.day < CURRENT_DATE - 35 OR NEW.day > CURRENT_DATE + 1 THEN
        RETURN NULL;
    END IF;
    IF EXISTS (
        SELECT 1 FROM cli_daily_contributor
        WHERE day = NEW.day AND dimension = NEW.dimension
          AND key = NEW.key AND device_id = NEW.device_id
    ) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('cli-contributor:' || NEW.day::text, 0));
    IF EXISTS (
        SELECT 1 FROM cli_daily_contributor
        WHERE day = NEW.day AND dimension = NEW.dimension
          AND key = NEW.key AND device_id = NEW.device_id
    ) THEN
        RETURN NEW;
    END IF;
    INSERT INTO cli_cardinality_admission(day, kind, used)
    VALUES (NEW.day, 'contributor', 1)
    ON CONFLICT (day, kind) DO UPDATE
    SET used = cli_cardinality_admission.used + 1
    WHERE cli_cardinality_admission.used < 50000
    RETURNING used INTO admitted;
    IF admitted IS NULL THEN
        INSERT INTO cli_aggregate_overflow(day)
        VALUES (NEW.day) ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;

DELETE FROM cli_cardinality_admission
WHERE kind LIKE 'counter:%' OR kind LIKE 'contributor:%';
INSERT INTO cli_cardinality_admission(day, kind, used)
SELECT day, 'counter', LEAST(count(*)::integer, 2000)
FROM cli_daily_counter GROUP BY day
ON CONFLICT (day, kind) DO UPDATE SET used = EXCLUDED.used;
INSERT INTO cli_cardinality_admission(day, kind, used)
SELECT day, 'contributor', LEAST(count(*)::integer, 50000)
FROM cli_daily_contributor GROUP BY day
ON CONFLICT (day, kind) DO UPDATE SET used = EXCLUDED.used;

DROP FUNCTION IF EXISTS cli_contributor_dimension_cap(text);
DROP FUNCTION IF EXISTS cli_counter_dimension_cap(text);

ALTER TABLE cli_aggregate_overflow DROP CONSTRAINT cli_aggregate_overflow_pkey;
UPDATE cli_aggregate_overflow SET dimension = '*';
DELETE FROM cli_aggregate_overflow a
USING cli_aggregate_overflow b
WHERE a.day = b.day AND a.ctid > b.ctid;
ALTER TABLE cli_aggregate_overflow DROP COLUMN dimension;
ALTER TABLE cli_aggregate_overflow ADD PRIMARY KEY (day);
