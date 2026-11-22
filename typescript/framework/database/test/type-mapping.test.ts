import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { Column, DateIso, Key, Repository, Table } from '../src/index';
import { database } from '../src/factory';
import { TYPES } from '../src/postgres/type-mapping';
import type { SqlClient } from '../src/sql-client';
import { ensurePostgresContainer, isDockerAvailable, POSTGRES_SETUP_TIMEOUT_MS } from './utils/postgres-helper';

/**
 * A `date`, `timestamp` or `timestamptz` value read back from PostgreSQL is the
 * instant that was written, whatever the time zone of the process. The driver
 * parses the text PostgreSQL sends, so a parser that takes a zone-less text as
 * local time shifts `timestamp` values by the host's offset as soon as TZ is
 * not UTC. These suites switch the process zone to prove it does not.
 */

// Zones east and west of UTC, one with a half-hour offset.
const NON_UTC_ZONES = ['Europe/Paris', 'America/Los_Angeles', 'Asia/Kolkata'];

/**
 * Sets the process time zone for the enclosing describe block, then restores
 * the zone in effect before it. Bun applies an assignment to TZ immediately.
 * `bun test` runs in UTC while TZ is unset, and deleting TZ would switch to the
 * system zone instead, so the zone is restored by name.
 */
function useTimeZone(zone: string): void {
  let previous = '';
  beforeAll(() => {
    previous = process.env.TZ ?? Intl.DateTimeFormat().resolvedOptions().timeZone;
    process.env.TZ = zone;
  });
  afterAll(() => {
    process.env.TZ = previous;
  });
}

/**
 * Instants paired with the text PostgreSQL stores and sends back for them in a
 * `timestamp` column: the UTC wall clock, space-separated, trailing zeros of
 * the fraction dropped.
 */
const TIMESTAMPS = [
  { instant: '2026-11-22T10:30:00.123Z', text: '2026-11-22 10:30:00.123' },
  // Summer time, a millisecond before a midnight that Paris and Kolkata have passed.
  { instant: '2026-07-14T23:59:59.999Z', text: '2026-07-14 23:59:59.999' },
  // Wall clocks Paris and Los Angeles skip when they move to summer time.
  { instant: '2026-03-29T02:30:00.000Z', text: '2026-03-29 02:30:00' },
  { instant: '2026-03-08T02:30:00.000Z', text: '2026-03-08 02:30:00' },
];

/**
 * Further texts PostgreSQL renders for a `timestamp`, each paired with the
 * instant it names: a fraction finer or coarser than a millisecond, a year
 * below 100, a BC year (44 BC is year -43) and a five-digit year.
 */
const TIMESTAMP_TEXTS = [
  { text: '2026-11-22 10:30:00.123456', instant: '2026-11-22T10:30:00.123Z' },
  { text: '2026-11-22 10:30:00.5', instant: '2026-11-22T10:30:00.500Z' },
  { text: '0044-03-15 12:00:00', instant: '0044-03-15T12:00:00.000Z' },
  { text: '0044-03-15 12:00:00 BC', instant: '-000043-03-15T12:00:00.000Z' },
  { text: '10000-01-01 00:00:00', instant: '+010000-01-01T00:00:00.000Z' },
];

/**
 * Texts without a zone, bound as a `timestamp` or `date` parameter, each paired
 * with the clock and the date PostgreSQL stores for it. postgres.js serializes
 * a bound string with the serializer of the type the server describes for the
 * parameter, so these reach `serialize`. A text that `new Date` took as local
 * time would store a clock shifted by the host's offset: Paris and Kolkata
 * would move `00:00` to the previous day, Los Angeles would move `23:30` to the
 * next, and none of them could read a BC year.
 */
const ZONELESS_TEXTS = [
  { text: '2026-11-22 10:30:00', clock: '2026-11-22 10:30:00', day: '2026-11-22' },
  { text: '2026-11-22 00:00', clock: '2026-11-22 00:00:00', day: '2026-11-22' },
  { text: '2026-11-22T23:30', clock: '2026-11-22 23:30:00', day: '2026-11-22' },
  // Microseconds, which a Date would truncate to the millisecond.
  { text: '2026-11-22T10:30:00.123456', clock: '2026-11-22 10:30:00.123456', day: '2026-11-22' },
  // A wall clock Paris skips when it moves to summer time.
  { text: '2026-03-29 02:30:00', clock: '2026-03-29 02:30:00', day: '2026-03-29' },
  { text: '2026-11-22', clock: '2026-11-22 00:00:00', day: '2026-11-22' },
  { text: '0044-03-15 12:00:00 BC', clock: '0044-03-15 12:00:00 BC', day: '0044-03-15 BC' },
  { text: '10000-01-01 00:00:00', clock: '10000-01-01 00:00:00', day: '10000-01-01' },
];

describe.each(NON_UTC_ZONES)('type mapping under TZ=%s', (zone) => {
  useTimeZone(zone);

  it('runs in the zone under test', () => {
    expect(Intl.DateTimeFormat().resolvedOptions().timeZone).toBe(zone);
    expect(new Date('2026-11-22T10:30:00Z').getTimezoneOffset()).not.toBe(0);
  });

  it.each(TIMESTAMPS)('round-trips the timestamp $instant as the UTC wall clock', ({ instant, text }) => {
    expect(TYPES.timestamp.serialize(new Date(instant))).toBe(instant.replace('Z', ''));
    expect(TYPES.timestamp.parse(text).toISOString()).toBe(instant);
  });

  it.each(TIMESTAMP_TEXTS)('reads the timestamp text $text as $instant', ({ text, instant }) => {
    expect(TYPES.timestamp.parse(text).toISOString()).toBe(instant);
  });

  it('reads infinity and -infinity as an Invalid Date, as for date and timestamptz', () => {
    for (const text of ['infinity', '-infinity']) {
      expect(Number.isNaN(TYPES.timestamp.parse(text).getTime())).toBe(true);
      expect(Number.isNaN(TYPES.date.parse(text).getTime())).toBe(true);
      expect(Number.isNaN(TYPES.timestamptz.parse(text).getTime())).toBe(true);
    }
  });

  it.each(ZONELESS_TEXTS)('writes the zone-less text $text to timestamp and date as written', ({ text }) => {
    expect(TYPES.timestamp.serialize(text)).toBe(text);
    expect(TYPES.date.serialize(text)).toBe(text);
  });

  it('writes a zoned text or a number as the UTC clock and date of its instant', () => {
    const cases = [
      { value: '2026-11-22T00:30:00.123Z', clock: '2026-11-22T00:30:00.123', day: '2026-11-22' },
      { value: '2026-11-22T00:30:00.123+01:00', clock: '2026-11-21T23:30:00.123', day: '2026-11-21' },
      { value: Date.parse('2026-11-22T00:30:00.123Z'), clock: '2026-11-22T00:30:00.123', day: '2026-11-22' },
    ];
    for (const { value, clock, day } of cases) {
      expect(TYPES.timestamp.serialize(value)).toBe(clock);
      expect(TYPES.date.serialize(value)).toBe(day);
    }
  });

  it('round-trips a date as UTC midnight', () => {
    expect(TYPES.date.serialize(new Date('2026-11-22T00:00:00.000Z'))).toBe('2026-11-22');
    expect(TYPES.date.parse('2026-11-22').toISOString()).toBe('2026-11-22T00:00:00.000Z');
  });

  it('round-trips a timestamptz whatever offset the session renders it in', () => {
    const instant = '2026-11-22T10:30:00.123Z';
    expect(TYPES.timestamptz.serialize(new Date(instant))).toBe(instant);
    for (const text of ['2026-11-22 10:30:00.123+00', '2026-11-22 11:30:00.123+01', '2026-11-22 16:00:00.123+05:30']) {
      expect(TYPES.timestamptz.parse(text).toISOString()).toBe(instant);
    }
  });
});

const DB_NAME = 'sql-test';
const dockerAvailable = await isDockerAvailable();
const TABLE = `type_mapping_${Date.now()}_${process.pid}`;

const EventsTable = Table(
  TABLE,
  {
    id: Key(String),
    occurredAt: Column(DateIso, { columnName: 'occurred_at' }),
  },
  { db: DB_NAME },
);

class EventRepository extends Repository<typeof EventsTable> {
  constructor() {
    super(EventsTable);
  }
}

describe.skipIf(!dockerAvailable)('type mapping against PostgreSQL under TZ=Europe/Paris', () => {
  useTimeZone('Europe/Paris');
  let sql: SqlClient;

  beforeAll(async () => {
    await ensurePostgresContainer(DB_NAME);
    sql = await database(DB_NAME);
    await sql.unsafe(
      `CREATE TABLE "${TABLE}" (id TEXT PRIMARY KEY, occurred_at TIMESTAMP, day DATE, occurred_at_tz TIMESTAMPTZ)`,
    );
  }, POSTGRES_SETUP_TIMEOUT_MS);

  afterAll(async () => {
    await sql?.unsafe(`DROP TABLE IF EXISTS "${TABLE}"`);
  });

  it('gives back the instant written to timestamp, date and timestamptz columns', async () => {
    const instant = new Date('2026-11-22T10:30:00.123Z');
    const day = new Date('2026-11-22T00:00:00.000Z');
    // Typed parameters go through the framework serializers: a bare Date is
    // sent as timestamptz, which the server would convert in its own zone.
    await sql`
      INSERT INTO ${sql(TABLE)} (id, occurred_at, day, occurred_at_tz)
      VALUES ('typed', ${sql.typed(instant, 1114)}, ${sql.typed(day, 1082)}, ${instant})
    `;

    const [row] = await sql<{ occurredAt: Date; storedClock: string; day: Date; occurredAtTz: Date }[]>`
      SELECT occurred_at, occurred_at::text AS stored_clock, day, occurred_at_tz
      FROM ${sql(TABLE)} WHERE id = 'typed'
    `;
    expect(row?.storedClock).toBe('2026-11-22 10:30:00.123');
    expect(row?.occurredAt.toISOString()).toBe(instant.toISOString());
    expect(row?.day.toISOString()).toBe(day.toISOString());
    expect(row?.occurredAtTz.toISOString()).toBe(instant.toISOString());
  });

  it('stores a zone-less text bound to a timestamp or date column as the clock and date it names', async () => {
    // Untyped string parameters: the server describes them from their target
    // columns, so postgres.js writes them with the timestamp and date serializers.
    await sql`
      INSERT INTO ${sql(TABLE)} (id, occurred_at, day)
      VALUES ('zone-less', ${'2026-11-22 10:30:00'}, ${'2026-11-22 00:00'})
    `;

    const [row] = await sql<{ storedClock: string; storedDay: string }[]>`
      SELECT occurred_at::text AS stored_clock, day::text AS stored_day
      FROM ${sql(TABLE)} WHERE id = 'zone-less'
    `;
    expect(row?.storedClock).toBe('2026-11-22 10:30:00');
    expect(row?.storedDay).toBe('2026-11-22');
  });

  it('writes every zone-less text bound as a timestamp or a date as the clock and date it names', async () => {
    for (const { text, clock, day } of ZONELESS_TEXTS) {
      const [row] = await sql<{ clock: string; day: string }[]>`
        SELECT ${text}::timestamp::text AS clock, ${text}::date::text AS day
      `;
      expect(row).toEqual({ clock, day });
    }
  });

  it('reads every timestamp text PostgreSQL renders as the instant it names', async () => {
    for (const { text, instant } of TIMESTAMP_TEXTS) {
      // Literals, not parameters: a bound string would go through the
      // serializer, and this test is about what the parser receives.
      const [row] = await sql.unsafe<{ at: Date; rendered: string }[]>(
        `SELECT '${text}'::timestamp AS at, '${text}'::timestamp::text AS rendered`,
      );
      expect(row?.rendered).toBe(text);
      expect(row?.at.toISOString()).toBe(instant);
    }
  });

  it('reads a timestamp column into a DateIso field as the instant the Repository saved', async () => {
    const repository = new EventRepository();
    await repository.save({ id: 'saved', occurredAt: '2026-11-22T10:30:00.123Z' });

    const found = await repository.get({ id: 'saved' });
    expect(found?.occurredAt).toBe('2026-11-22T10:30:00.123Z');
  });
});
