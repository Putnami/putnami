CREATE TABLE cli_daily_contributor (
    day        date NOT NULL,
    dimension  text NOT NULL,
    key        text NOT NULL,
    device_id  text NOT NULL,
    PRIMARY KEY (day, dimension, key, device_id)
);

CREATE INDEX cli_daily_contributor_window_idx
    ON cli_daily_contributor (day, dimension, key);

CREATE TABLE cli_aggregate_overflow (
    day date PRIMARY KEY
);

CREATE TABLE cli_cardinality_admission (
    day  date NOT NULL,
    kind text NOT NULL,
    used integer NOT NULL CHECK (used >= 0),
    PRIMARY KEY (day, kind)
);

INSERT INTO cli_cardinality_admission(day, kind, used)
SELECT day, 'device', LEAST(count(*)::integer, 10000)
FROM cli_device_day GROUP BY day;

INSERT INTO cli_cardinality_admission(day, kind, used)
SELECT day, 'counter', LEAST(count(*)::integer, 2000)
FROM cli_daily_counter GROUP BY day;

INSERT INTO cli_aggregate_overflow(day)
SELECT day FROM cli_device_day GROUP BY day HAVING count(*) > 10000
ON CONFLICT DO NOTHING;

INSERT INTO cli_aggregate_overflow(day)
SELECT day FROM cli_daily_counter GROUP BY day HAVING count(*) > 2000
ON CONFLICT DO NOTHING;

CREATE TABLE cli_applied_flush (
    flush_id  text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (length(flush_id) = 32)
);

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
        INSERT INTO cli_aggregate_overflow(day) VALUES (NEW.day) ON CONFLICT DO NOTHING;
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
        INSERT INTO cli_aggregate_overflow(day) VALUES (NEW.day) ON CONFLICT DO NOTHING;
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
        INSERT INTO cli_aggregate_overflow(day) VALUES (NEW.day) ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER cli_device_day_bound
BEFORE INSERT ON cli_device_day
FOR EACH ROW EXECUTE FUNCTION cli_bound_device_day();

CREATE TRIGGER cli_daily_counter_bound
BEFORE INSERT ON cli_daily_counter
FOR EACH ROW EXECUTE FUNCTION cli_bound_daily_counter();

CREATE TRIGGER cli_daily_contributor_bound
BEFORE INSERT ON cli_daily_contributor
FOR EACH ROW EXECUTE FUNCTION cli_bound_daily_contributor();
