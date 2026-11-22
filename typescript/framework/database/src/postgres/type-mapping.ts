import postgres from 'postgres';

/**
 * The text PostgreSQL sends for a finite `timestamp` (without time zone) under
 * its default ISO DateStyle: `YYYY-MM-DD HH:MM:SS[.ffffff][ BC]`, the year
 * four digits or more.
 */
const TIMESTAMP_TEXT = /^(\d{4,})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,6}))?( BC)?$/;

/**
 * Reads a `timestamp` (without time zone) value as a UTC wall clock.
 *
 * `serialize` writes the UTC wall clock, so the read takes the stored clock as
 * UTC too. `new Date(text)` would take the space-separated form as the local
 * time of the process and shift every value by the host's offset whenever TZ
 * is not UTC. The Go adapter (pgx) reads `timestamp` as UTC the same way.
 *
 * The date is built from its fields rather than from a rewritten string, so a
 * five-digit year and a BC year (year 1 BC is year 0) read as well. Fractional
 * seconds are truncated to the millisecond a `Date` holds. `infinity` and
 * `-infinity` have no `Date` and read as an Invalid Date, as they do for
 * `date` and `timestamptz`.
 */
function parseTimestamp(text: string): Date {
  const match = TIMESTAMP_TEXT.exec(text);
  if (!match) return new Date(Number.NaN);
  const [, year, month, day, hours, minutes, seconds, fraction = '', bc] = match;
  const date = new Date(0);
  // setUTCFullYear, unlike Date.UTC, does not read years 0 to 99 as 1900 to 1999.
  date.setUTCFullYear(bc ? 1 - Number(year) : Number(year), Number(month) - 1, Number(day));
  date.setUTCHours(Number(hours), Number(minutes), Number(seconds), Number(fraction.padEnd(3, '0').slice(0, 3)));
  return date;
}

/**
 * A date or timestamp text that carries no zone, in the ISO form PostgreSQL
 * reads whatever its DateStyle: `YYYY-MM-DD`, optionally followed by `T` or a
 * space and `HH:MM[:SS[.fraction]]`, optionally followed by ` BC`, the year
 * four digits or more.
 */
const ZONELESS_TEXT = /^\d{4,}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?)?(?: BC)?$/;

/**
 * Tells whether a `date` or `timestamp` parameter is a zone-less ISO text.
 *
 * postgres.js serializes a bound string with the serializer of the type the
 * server describes for the parameter, so a string bound to a `date` or
 * `timestamp` column reaches `serialize`. A zone-less text names a calendar
 * date or a wall clock, not an instant: `serialize` sends it as written, and
 * the server stores that date or clock whatever the time zone of the process.
 * `new Date(text)` would take it as local time, shift it by the host's offset,
 * truncate its microseconds and reject a BC year.
 */
function isZonelessText(x: unknown): x is string {
  return typeof x === 'string' && ZONELESS_TEXT.test(x);
}

export const TYPES = {
  bigint: postgres.BigInt,
  numeric: {
    to: 1700,
    from: [1700],
    // NUMERIC is an arbitrary-precision decimal. Parsing through
    // `Number.parseFloat` would silently round money / high-precision values
    // to an IEEE-754 double. Keep the lossless string the server sent; the
    // repository coerces it back to a JS number only for columns whose schema
    // type is numeric (`Number`/`Int`), so callers who want exact decimals can
    // declare the column as `String`. See EntityHelper.toEntity.
    parse: (x: string) => x,
    serialize: (x: number | string | bigint) => x.toString(),
  },
  date: {
    to: 1082,
    from: [1082],
    parse: (x: string) => new Date(x),
    serialize: (x: Date | number | string) => {
      if (isZonelessText(x)) return x;
      const date = x instanceof Date ? x : new Date(x);
      return date?.toISOString().split('T')[0];
    },
  },
  timestamp: {
    to: 1114,
    from: [1114],
    parse: parseTimestamp,
    serialize: (x: Date | number | string) => {
      if (isZonelessText(x)) return x;
      const date = x instanceof Date ? x : new Date(x);
      return date?.toISOString().replace('Z', '');
    },
  },
  timestamptz: {
    to: 1184,
    from: [1184],
    parse: (x: string) => new Date(x),
    serialize: (x: Date) => {
      const date = x instanceof Date ? x : new Date(x);
      return date?.toISOString();
    },
  },
  json: {
    to: 114,
    from: [114, 3802], // 114 = json, 3802 = jsonb
    parse: (x: string) => JSON.parse(x),
    serialize: (x: unknown) => JSON.stringify(x),
  },
  vector: {
    // pgvector OID is dynamically assigned - use 0 to let fetch_types detect it
    to: 0,
    from: [] as number[], // Will be populated by fetch_types
    parse: (x: string): number[] => {
      const trimmed = x.replace(/^\[|\]$/g, '');
      return trimmed ? trimmed.split(',').map(Number) : [];
    },
    serialize: (x: number[]): string => `[${x.join(',')}]`,
  },
};
