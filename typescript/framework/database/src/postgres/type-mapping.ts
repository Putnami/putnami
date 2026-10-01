import postgres from 'postgres';

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
    serialize: (x: Date | number) => {
      const date = x instanceof Date ? x : new Date(x);
      return date?.toISOString().split('T')[0];
    },
  },
  timestamp: {
    to: 1114,
    from: [1114],
    parse: (x: string) => new Date(x),
    serialize: (x: Date | number) => {
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
