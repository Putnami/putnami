import { truncateUtf8 } from './enrich/referrer';
import { ACTION_NAME_RE, MAX_PROP_KEYS, MAX_PROP_STRING_LEN, PROP_KEY_RE } from './sanitize/vocabulary';

/** The three primitive constructors a declared property may be typed with. */
export type PropType = StringConstructor | NumberConstructor | BooleanConstructor;

/**
 * Property schema of a declared action event.
 *
 * Each key is a property name; the value is the constructor of the accepted
 * primitive (`String`, `Number`, `Boolean`). The declaration is the closed
 * vocabulary the ingest route validates against, and the allowlist
 * {@link validateProps} projects an untrusted payload through.
 */
export type PropsSchema = Record<string, PropType>;

/** Declared action events, keyed by event name. */
export type DeclaredEvents = Record<string, PropsSchema>;

/** What {@link registerDeclared} hands the runtime. */
export interface RegisteredEvents {
  /** The declared action names, the set the sanitizer filters the wire with. */
  names: ReadonlySet<string>;
  /** The per-name property schemas, the allowlist `validateProps` applies. */
  schemas: DeclaredEvents;
}

/**
 * Declares the action events an application records, validating every name and
 * property key against the wire contract at the point of declaration.
 *
 * Declaring up front is what makes an analytics payload bounded: the browser
 * can only ever move a name that already exists here, so an attacker — or a
 * careless `track()` — cannot mint an unbounded series of counter keys. The
 * check runs at import time rather than at ingest so a typo is a startup
 * error a developer sees, not a silent drop in production.
 *
 * @param events - The declared events and their property schemas.
 * @returns The same map, so the declaration can be exported and reused.
 * @throws Error naming the offending event, property, or type.
 */
export function declareEvents<T extends DeclaredEvents>(events: T): T {
  for (const [name, schema] of Object.entries(events)) {
    if (!ACTION_NAME_RE.test(name)) {
      throw new Error(`analytics: action name "${name}" must match ${ACTION_NAME_RE.source}`);
    }
    const keys = Object.keys(schema);
    if (keys.length > MAX_PROP_KEYS) {
      throw new Error(
        `analytics: action "${name}" declares ${keys.length} properties, the maximum is ${MAX_PROP_KEYS}`,
      );
    }
    for (const key of keys) {
      if (!PROP_KEY_RE.test(key)) {
        throw new Error(`analytics: property "${key}" of action "${name}" must match ${PROP_KEY_RE.source}`);
      }
      const type = schema[key];
      if (type !== String && type !== Number && type !== Boolean) {
        throw new Error(`analytics: property "${key}" of action "${name}" must be typed String, Number, or Boolean`);
      }
    }
  }
  return events;
}

/**
 * Validates a declaration and splits it into the two shapes the request path
 * needs: the name set the sanitizer filters with, and the schemas the props
 * allowlist is applied from.
 *
 * @param events - The declaration, from `analytics({ events })` or `declareEvents`.
 * @returns The declared names and their schemas.
 * @throws Error when a name, a property key, or a property type is invalid.
 */
export function registerDeclared(events: DeclaredEvents): RegisteredEvents {
  const schemas = declareEvents({ ...events });
  return { names: new Set(Object.keys(schemas)), schemas };
}

/**
 * Projects an untrusted property payload through a declared schema.
 *
 * The iteration is over the *schema*, never over the payload: a key nobody
 * declared has no way of reaching the result, so an event carrying a session
 * token or an e-mail address under an inventive key stores nothing. A declared
 * key whose value has the wrong primitive type is dropped the same way —
 * storing a number where a string was declared would poison every later read
 * of that property.
 *
 * @param schema - The declared schema, or undefined for an unknown action.
 * @param props - The candidate properties, from the wire or from `track()`.
 * @returns Only the declared keys whose value matched the declared type.
 */
export function validateProps(
  schema: PropsSchema | undefined,
  props: Record<string, unknown> | undefined,
): Record<string, string | number | boolean> {
  const validated: Record<string, string | number | boolean> = {};
  if (!schema || !props) {
    return validated;
  }
  for (const [key, type] of Object.entries(schema)) {
    const value = props[key];
    if (type === String && typeof value === 'string') {
      validated[key] = truncateUtf8(value, MAX_PROP_STRING_LEN);
    } else if (type === Number && typeof value === 'number' && Number.isFinite(value)) {
      validated[key] = value;
    } else if (type === Boolean && typeof value === 'boolean') {
      validated[key] = value;
    }
  }
  return validated;
}
