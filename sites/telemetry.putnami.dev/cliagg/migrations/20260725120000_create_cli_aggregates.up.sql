CREATE TABLE cli_device_day (
    day       date NOT NULL,
    device_id text NOT NULL,
    PRIMARY KEY (day, device_id)
);

CREATE TABLE cli_daily_counter (
    day       date   NOT NULL,
    dimension text   NOT NULL,
    key       text   NOT NULL,
    count     bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (day, dimension, key)
);
