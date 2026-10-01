/**
 * The TypeScript twin of `protocols/analytics/analytics.go`.
 *
 * Every constant, enum, and regular expression here is a copy of the Go
 * contract, because the Go package is the specification and this file is the
 * only place the copy is made. `test/vocabulary-guard.test.ts` reads the Go
 * source as text and fails when a value drifts, so a bound that exists only
 * inside an `if` — or a regex retyped from memory — cannot silently diverge.
 */

/** The only accepted value of a batch's `protocolVersion`. */
export const PROTOCOL_VERSION = 1;

/** The largest number of events one batch may carry (Go `MaxEvents`). */
export const MAX_EVENTS = 50;
/** The largest accepted request body in bytes (Go `MaxBodyBytes`). */
export const MAX_BODY_BYTES = 65_536;
/** The highest per-session sequence number (Go `MaxSeq`). */
export const MAX_SEQ = 1_000_000;
/** Engagement time ceiling, 24 hours (Go `MaxEngagementMs`). */
export const MAX_ENGAGEMENT_MS = 86_400_000;
/** Byte bound of `page.path` (Go `MaxPathLen`). */
export const MAX_PATH_LEN = 512;
/** Byte bound of `page.route` (Go `MaxRouteLen`). */
export const MAX_ROUTE_LEN = 256;
/** Byte bound of `page.referrer` (Go `MaxReferrerLen`). */
export const MAX_REFERRER_LEN = 512;
/** Byte bound of each `page.utm` value (Go `MaxUTMLen`). */
export const MAX_UTM_LEN = 128;
/** Byte bound of an action name (Go `MaxActionNameLen`). */
export const MAX_ACTION_NAME_LEN = 64;
/** The largest number of action properties (Go `MaxPropKeys`). */
export const MAX_PROP_KEYS = 20;
/** Byte bound of an action property key (Go `MaxPropKeyLen`). */
export const MAX_PROP_KEY_LEN = 32;
/** Byte bound of a string-valued action property (Go `MaxPropStringLen`). */
export const MAX_PROP_STRING_LEN = 256;

/** Event name of one viewed page. */
export const EVENT_PAGE_VIEW = 'page_view';
/** Event name of one declared, server-known action. */
export const EVENT_ACTION = 'action';
/** Server-only event name; a wire batch carrying it is rejected. */
export const EVENT_FORM_SUBMIT = 'form_submit';

/** Client viewport buckets (Go `ViewportClasses`). */
export const VIEWPORT_CLASSES = ['xs', 'sm', 'md', 'lg', 'xl'] as const;
/** Referrer classifications (Go `ReferrerTypes`). */
export const REFERRER_TYPES = ['direct', 'internal', 'search', 'social', 'other'] as const;
/** Device classifications (Go `DeviceTypes`). */
export const DEVICE_TYPES = ['desktop', 'mobile', 'tablet', 'other'] as const;
/** Browser families (Go `Browsers`). */
export const BROWSERS = ['chrome', 'safari', 'firefox', 'edge', 'opera', 'samsung', 'other'] as const;
/** Operating-system families (Go `OperatingSystems`). */
export const OPERATING_SYSTEMS = ['windows', 'macos', 'ios', 'android', 'linux', 'chromeos', 'other'] as const;
/** Whether the server or the browser emitted a record (Go `Sources`). */
export const SOURCES = ['server', 'client'] as const;
/** How a visitor id was derived (Go `VisitorKinds`). */
export const VISITOR_KINDS = ['daily', 'cookie'] as const;
/** Recorded form-submission outcomes (Go `Outcomes`). */
export const OUTCOMES = ['ok', 'validation_error', 'error'] as const;
/** The accepted campaign parameters (Go `UTMKeys`). */
export const UTM_KEYS = ['source', 'medium', 'campaign', 'content', 'term'] as const;
/** The daily counter dimensions (Go `Dimensions`, body §B.2). */
export const DIMENSIONS = [
  'event',
  'route',
  'path',
  'referrer_host',
  'referrer_type',
  'utm_source',
  'utm_medium',
  'utm_campaign',
  'country',
  'device_type',
  'browser',
  'os',
  'language',
  'action',
  'form_submit',
] as const;

/**
 * The columns of `analytics_event` (body §B.1), in declaration order.
 *
 * The guard test asserts that no member of this list can carry an IP address,
 * a raw user agent, a query string, a page title, or a credential: what a
 * schema does not have a column for cannot be stored by accident.
 */
export const EVENT_COLUMNS = [
  'event_id',
  'received_at',
  'ts',
  'day',
  'name',
  'source',
  'app',
  'env',
  'app_version',
  'visitor_id',
  'visitor_kind',
  'session_id',
  'seq',
  'user_id',
  'route',
  'path',
  'referrer',
  'referrer_type',
  'utm_source',
  'utm_medium',
  'utm_campaign',
  'utm_content',
  'utm_term',
  'status_code',
  'render_ms',
  'engagement_ms',
  'action_name',
  'outcome',
  'props',
  'browser',
  'browser_major',
  'os',
  'device_type',
  'language',
  'viewport_class',
  'country',
] as const;

/**
 * The closed drop-reason vocabulary.
 *
 * The first eighteen are the Go error codes minus the `analytics.` prefix (
 * `parse_error` excepted: a body that is not JSON never reaches the sanitizer).
 * `unknown_action`, `bot`, `rate_limited`, `duplicate`, and `overflow` are
 * server-side reasons Go cannot know: only the application declares its action
 * names, and only the server sees bots, rate limits, duplicates, and a write
 * queue that filled up.
 */
export const DROP_REASONS = [
  'invalid_version',
  'invalid_timestamp',
  'batch_too_large',
  'invalid_event_id',
  'unknown_event',
  'unknown_attribute',
  'missing_attribute',
  'attribute_kind',
  'invalid_value',
  'invalid_path',
  'invalid_route',
  'invalid_referrer',
  'invalid_utm',
  'props_too_many',
  'invalid_prop_key',
  'invalid_prop_value',
  'invalid_action',
  'unknown_action',
  'bot',
  'rate_limited',
  'duplicate',
  'overflow',
] as const;

/** RFC 3339 with milliseconds and a literal Z (Go `TimestampRe`). */
export const TIMESTAMP_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;
/** Lowercase UUID version 7 with an RFC 4122 variant (Go `UUIDv7Re`). */
export const UUID_V7_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
/** Putnami route pattern with `[param]` and `[...rest]` (Go `RouteRe`). */
export const ROUTE_RE =
  /^\/([A-Za-z0-9._~-]+|\[[A-Za-z0-9_]+\]|\[\.\.\.[A-Za-z0-9_]+\])(\/([A-Za-z0-9._~-]+|\[[A-Za-z0-9_]+\]|\[\.\.\.[A-Za-z0-9_]+\]))*$|^\/$/;
/** Declared action name (Go `ActionNameRe`). */
export const ACTION_NAME_RE = /^[a-z][a-z0-9_]{0,63}$/;
/** Action property key (Go `PropKeyRe`). */
export const PROP_KEY_RE = /^[a-z][a-z0-9_]{0,31}$/;
/** BCP 47 primary tag with an optional subtag (Go `LanguageRe`). */
export const LANGUAGE_RE = /^[a-z]{2,3}(-[A-Za-z0-9]{2,8})?$/;

/** One viewport bucket. */
export type ViewportClass = (typeof VIEWPORT_CLASSES)[number];
/** One referrer classification. */
export type ReferrerType = (typeof REFERRER_TYPES)[number];
/** One device classification. */
export type DeviceType = (typeof DEVICE_TYPES)[number];
/** One browser family. */
export type Browser = (typeof BROWSERS)[number];
/** One operating-system family. */
export type OS = (typeof OPERATING_SYSTEMS)[number];
/** One campaign parameter name. */
export type UtmKey = (typeof UTM_KEYS)[number];
/** One reason an event or a batch was dropped. */
export type DropReason = (typeof DROP_REASONS)[number];

/** The five campaign parameters, each absent or a bounded string. */
export type UtmFields = Partial<Record<UtmKey, string>>;

/**
 * Length of a value in UTF-8 bytes.
 *
 * Every bound in the contract is a byte bound, because Go applies `len()` to a
 * string. `value.length` counts UTF-16 code units, so a 300-character Cyrillic
 * path measures 300 there and 600 here — a sanitizer using it would accept
 * documents the Go validator rejects, and no ASCII fixture could reveal it.
 *
 * @param value - The string to measure.
 * @returns The number of bytes the string occupies when UTF-8 encoded.
 */
export function utf8Bytes(value: string): number {
  return Buffer.byteLength(value, 'utf8');
}

/** Reports whether `value` is a known viewport class. */
export function isViewportClass(value: string): value is ViewportClass {
  return (VIEWPORT_CLASSES as readonly string[]).includes(value);
}

/** Reports whether `value` is an accepted campaign parameter name. */
export function isUtmKey(value: string): value is UtmKey {
  return (UTM_KEYS as readonly string[]).includes(value);
}

/** Reports whether `name` is an event name accepted on the wire. */
export function isWireEventName(name: string): boolean {
  return name === EVENT_PAGE_VIEW || name === EVENT_ACTION;
}
