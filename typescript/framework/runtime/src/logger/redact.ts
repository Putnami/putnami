// Secret redaction for structured logging.
//
// The logger receives arbitrary values via `logger.info('db', config)` and
// `.with('auth', token)`. Those values frequently carry credentials
// (`password`, `token`, `apiKey`, …) that must never reach stdout / Cloud
// Logging in plaintext. Config objects validated by a `Sensitive`-marked
// schema produce plain runtime objects — the sensitivity marker lives on the
// schema, not the value — so the log path masks by key name instead.
//
// Masking happens in `buildEntry`, before any sink runs, so every sink
// (JSON, console, buffered) emits redacted entries. Redaction always returns
// fresh objects/arrays so the caller's data and the live request `logContext`
// are never mutated.

/** Replacement written in place of a sensitive value. */
export const REDACTED = '***';

/** Default case-insensitive key denylist. */
const DEFAULT_SENSITIVE_KEYS: readonly string[] = ['password', 'token', 'apikey', 'authorization', 'secret'];
const CIRCULAR = '[Circular]';

let sensitiveKeys = new Set<string>(DEFAULT_SENSITIVE_KEYS);

/**
 * Replace the redaction key denylist. Keys are matched case-insensitively
 * against object property names. Passing no argument restores the defaults.
 */
export function setSensitiveKeys(keys?: readonly string[]): void {
  sensitiveKeys = new Set((keys ?? DEFAULT_SENSITIVE_KEYS).map((k) => k.toLowerCase()));
}

/** Add extra keys to the redaction denylist without dropping the defaults. */
export function addSensitiveKeys(...keys: string[]): void {
  for (const key of keys) {
    sensitiveKeys.add(key.toLowerCase());
  }
}

/** True when a property name should have its value masked. */
function isSensitiveKey(key: string): boolean {
  return sensitiveKeys.has(key.toLowerCase());
}

/**
 * Recursively redact sensitive values, returning a fresh structure. Objects
 * and arrays are cloned; any property whose key is on the denylist is replaced
 * with {@link REDACTED} regardless of its value type. Non-plain values
 * (Error, Date, primitives, …) are returned as-is.
 */
export function redact(value: unknown, seen: WeakSet<object> = new WeakSet()): unknown {
  if (value === null || typeof value !== 'object') {
    return value;
  }

  // Guard against circular references without returning the original object,
  // which could still carry plaintext secrets.
  if (seen.has(value)) {
    return CIRCULAR;
  }

  if (Array.isArray(value)) {
    seen.add(value);
    const result = value.map((item) => redact(item, seen));
    seen.delete(value);
    return result;
  }

  // Only descend into plain objects; leave Error/Date/Map/etc. intact so the
  // sink can serialize them as it does today.
  if (!isPlainObject(value)) {
    return value;
  }

  seen.add(value);
  const result: Record<string, unknown> = {};
  for (const [key, val] of Object.entries(value)) {
    result[key] = isSensitiveKey(key) ? REDACTED : redact(val, seen);
  }
  seen.delete(value);
  return result;
}

function isPlainObject(value: object): value is Record<string, unknown> {
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}
