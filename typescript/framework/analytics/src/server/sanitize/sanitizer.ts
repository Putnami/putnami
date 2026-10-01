import {
  ACTION_NAME_RE,
  type DropReason,
  EVENT_ACTION,
  EVENT_PAGE_VIEW,
  isUtmKey,
  isViewportClass,
  isWireEventName,
  LANGUAGE_RE,
  MAX_ENGAGEMENT_MS,
  MAX_EVENTS,
  MAX_PATH_LEN,
  MAX_PROP_KEYS,
  MAX_PROP_STRING_LEN,
  MAX_REFERRER_LEN,
  MAX_ROUTE_LEN,
  MAX_SEQ,
  MAX_UTM_LEN,
  PROP_KEY_RE,
  PROTOCOL_VERSION,
  ROUTE_RE,
  TIMESTAMP_RE,
  utf8Bytes,
  type UtmFields,
  UUID_V7_RE,
} from './vocabulary';

/** A page view that passed every rule of the wire contract. */
export interface SanitizedPageView {
  kind: 'page_view';
  eventId: string;
  clientTs: Date;
  seq: number;
  sessionId: string;
  engagementMs: number | null;
  viewportClass: string | null;
  language: string | null;
  path: string;
  route: string | null;
  referrer: string | null;
  utm: UtmFields;
}

/** A declared action that passed every rule of the wire contract. */
export interface SanitizedAction {
  kind: 'action';
  eventId: string;
  clientTs: Date;
  seq: number;
  sessionId: string;
  engagementMs: number | null;
  viewportClass: string | null;
  language: string | null;
  action: string;
  props: Record<string, string | number | boolean>;
}

/** One accepted event, discriminated by `kind`. */
export type SanitizedEvent = SanitizedPageView | SanitizedAction;

/** One rejection: the reason, and the event index it happened at (null = the whole batch). */
export interface SanitizeDrop {
  reason: DropReason;
  index: number | null;
}

/** What the sanitizer accepted, and every reason it rejected the rest. */
export interface SanitizeResult {
  sentAt: Date | null;
  events: SanitizedEvent[];
  dropped: SanitizeDrop[];
}

const BATCH_KEYS = new Set(['protocolVersion', 'sentAt', 'events']);
const EVENT_KEYS = new Set([
  'eventId',
  'name',
  'clientTs',
  'seq',
  'sessionId',
  'engagementMs',
  'viewportClass',
  'language',
  'page',
  'action',
  'props',
]);
const PAGE_KEYS = new Set(['path', 'route', 'referrer', 'utm']);

interface PageDraft {
  path: string;
  route: string | null;
  referrer: string | null;
  utm: UtmFields;
}

interface EventDraft {
  eventId: string;
  name: string;
  clientTs: Date | null;
  sessionId: string;
  seq: number;
  engagementMs: number | null;
  viewportClass: string | null;
  language: string | null;
  page?: PageDraft;
  action: string;
  props?: Record<string, string | number | boolean>;
}

/**
 * Validates one analytics batch against the wire contract and returns the
 * events that survived.
 *
 * It is the executable twin of `ValidateBatch` in `protocols/analytics`: the
 * same rules, in the same order, reporting the same reason names minus the
 * `analytics.` prefix. Like the Go validator it never stops at the first
 * violation, because the drop reasons are the metric, and it never throws:
 * the caller answers `202` either way, so a malformed document must be a
 * counted rejection rather than a request-path exception.
 *
 * @param input - The parsed request body, from an untrusted browser.
 * @param declared - The action names the application declared server-side.
 * @returns The batch send time, the accepted events, and every drop.
 */
export function sanitizeBatch(input: unknown, declared: ReadonlySet<string>): SanitizeResult {
  try {
    return sanitizeDocument(input, declared);
  } catch {
    // A document the sanitizer cannot even read is a kind failure of the
    // document as a whole. Reaching here means a hostile or exotic object,
    // never a JSON.parse result.
    return { sentAt: null, events: [], dropped: [{ reason: 'attribute_kind', index: null }] };
  }
}

function sanitizeDocument(input: unknown, declared: ReadonlySet<string>): SanitizeResult {
  if (!isPlainObject(input) || hasUnknownKeys(input, BATCH_KEYS)) {
    return { sentAt: null, events: [], dropped: [{ reason: 'unknown_attribute', index: null }] };
  }

  const envelope: DropReason[] = [];
  if (input['protocolVersion'] !== PROTOCOL_VERSION) {
    envelope.push('invalid_version');
  }
  const sentAt = parseTimestamp(input['sentAt']);
  if (sentAt === null) {
    envelope.push('invalid_timestamp');
  }
  const events = input['events'];
  if (!Array.isArray(events) || events.length === 0 || events.length > MAX_EVENTS) {
    envelope.push('batch_too_large');
  }
  if (envelope.length > 0 || !Array.isArray(events)) {
    return { sentAt, events: [], dropped: envelope.map((reason) => ({ reason, index: null })) };
  }

  const accepted: SanitizedEvent[] = [];
  const dropped: SanitizeDrop[] = [];
  for (let index = 0; index < events.length; index++) {
    const reasons: DropReason[] = [];
    const event = sanitizeEvent(events[index], declared, reasons);
    if (event === undefined) {
      dropped.push(...reasons.map((reason) => ({ reason, index })));
      continue;
    }
    accepted.push(event);
  }
  return { sentAt, events: accepted, dropped };
}

/** Validates one event, appending every violation to `reasons`. */
function sanitizeEvent(raw: unknown, declared: ReadonlySet<string>, reasons: DropReason[]): SanitizedEvent | undefined {
  if (!isPlainObject(raw)) {
    reasons.push('attribute_kind');
    return undefined;
  }
  if (hasUnknownKeys(raw, EVENT_KEYS)) {
    reasons.push('unknown_attribute');
  }
  const draft = readEvent(raw, declared, reasons);
  if (reasons.length > 0) {
    return undefined;
  }
  return buildEvent(draft, reasons);
}

/** Reads every member of an event, in the order the Go validator checks them. */
function readEvent(raw: Record<string, unknown>, declared: ReadonlySet<string>, reasons: DropReason[]): EventDraft {
  const eventId = readRequiredString(raw, 'eventId', reasons);
  const name = readRequiredString(raw, 'name', reasons);
  const clientTsRaw = readRequiredString(raw, 'clientTs', reasons);
  const sessionId = readRequiredString(raw, 'sessionId', reasons);
  if (eventId !== '' && !UUID_V7_RE.test(eventId)) {
    reasons.push('invalid_event_id');
  }
  if (name !== '' && !isWireEventName(name)) {
    reasons.push('unknown_event');
  }
  const clientTs = clientTsRaw === '' ? null : parseTimestamp(clientTsRaw);
  if (clientTsRaw !== '' && clientTs === null) {
    reasons.push('invalid_timestamp');
  }
  // A malformed sessionId reports invalid_event_id, as the Go validator does:
  // both members are the same UUID v7 rule and share its code.
  if (sessionId !== '' && !UUID_V7_RE.test(sessionId)) {
    reasons.push('invalid_event_id');
  }

  const action = readOptionalString(raw, 'action', reasons) ?? '';
  checkShape(raw, name, action, reasons);
  if (action !== '' && !ACTION_NAME_RE.test(action)) {
    reasons.push('invalid_action');
  } else if (name === EVENT_ACTION && action !== '' && !declared.has(action)) {
    // TypeScript-only rule: only the application knows which action names it
    // declared, so Go cannot express this one.
    reasons.push('unknown_action');
  }

  const scalars = readScalars(raw, reasons);
  const page = raw['page'] === undefined || raw['page'] === null ? undefined : readPage(raw['page'], reasons);
  const props = readProps(raw['props'], reasons);
  return { eventId, name, clientTs, sessionId, ...scalars, page, action, props };
}

/** Applies the presence rules of an event kind (Go `validateEventShape`). */
function checkShape(raw: Record<string, unknown>, name: string, action: string, reasons: DropReason[]): void {
  const hasPage = raw['page'] !== undefined && raw['page'] !== null;
  const hasProps = raw['props'] !== undefined && raw['props'] !== null;
  if (name === EVENT_PAGE_VIEW) {
    if (!hasPage) {
      reasons.push('missing_attribute');
    }
    if (action !== '') {
      reasons.push('unknown_attribute');
    }
    if (hasProps) {
      reasons.push('unknown_attribute');
    }
    return;
  }
  if (name === EVENT_ACTION) {
    if (hasPage) {
      reasons.push('unknown_attribute');
    }
    if (action === '') {
      reasons.push('missing_attribute');
    }
  }
}

/** Reads the bounded optional scalars of an event. */
function readScalars(
  raw: Record<string, unknown>,
  reasons: DropReason[],
): Pick<EventDraft, 'seq' | 'engagementMs' | 'viewportClass' | 'language'> {
  const seq = readInteger(raw['seq'], reasons) ?? 0;
  if (seq < 0 || seq > MAX_SEQ) {
    reasons.push('invalid_value');
  }
  const engagementMs = readInteger(raw['engagementMs'], reasons);
  if (engagementMs !== null && (engagementMs < 0 || engagementMs > MAX_ENGAGEMENT_MS)) {
    reasons.push('invalid_value');
  }
  const viewportClass = readOptionalString(raw, 'viewportClass', reasons);
  if (viewportClass !== null && !isViewportClass(viewportClass)) {
    reasons.push('invalid_value');
  }
  const language = readOptionalString(raw, 'language', reasons);
  if (language !== null && !LANGUAGE_RE.test(language)) {
    reasons.push('invalid_value');
  }
  return { seq, engagementMs, viewportClass, language };
}

/** Reads the `page` member of a page view (Go `validatePage`). */
function readPage(value: unknown, reasons: DropReason[]): PageDraft | undefined {
  if (!isPlainObject(value)) {
    reasons.push('attribute_kind');
    return undefined;
  }
  if (hasUnknownKeys(value, PAGE_KEYS)) {
    reasons.push('unknown_attribute');
  }
  const path = readPath(value['path'], reasons);
  const route = readRoute(value['route'], reasons);
  const referrer = readReferrer(value['referrer'], reasons);
  return { path, route, referrer, utm: readUtm(value['utm'], reasons) };
}

/**
 * Reads `page.path`.
 *
 * A path beyond the bound rejects the event rather than being truncated: the
 * client controls the value and is the one that can fix it, and the Go
 * validator rejects it too. Truncation belongs to `normalizePath`, which folds
 * a server-derived URL nobody can be asked to shorten.
 */
function readPath(value: unknown, reasons: DropReason[]): string {
  if (value === undefined || value === null || value === '') {
    reasons.push('invalid_path');
    return '';
  }
  if (typeof value !== 'string') {
    reasons.push('attribute_kind');
    return '';
  }
  if (!value.startsWith('/') || value.includes('?') || value.includes('#') || utf8Bytes(value) > MAX_PATH_LEN) {
    reasons.push('invalid_path');
    return '';
  }
  return value;
}

/** Reads `page.route`. */
function readRoute(value: unknown, reasons: DropReason[]): string | null {
  if (value === undefined || value === null || value === '') {
    return null;
  }
  if (typeof value !== 'string') {
    reasons.push('attribute_kind');
    return null;
  }
  if (utf8Bytes(value) > MAX_ROUTE_LEN || !ROUTE_RE.test(value)) {
    reasons.push('invalid_route');
    return null;
  }
  return value;
}

/** Reads `page.referrer`. */
function readReferrer(value: unknown, reasons: DropReason[]): string | null {
  if (value === undefined || value === null || value === '') {
    return null;
  }
  if (typeof value !== 'string') {
    reasons.push('attribute_kind');
    return null;
  }
  if (!isWireReferrer(value)) {
    reasons.push('invalid_referrer');
    return null;
  }
  return value;
}

/**
 * Reports whether a wire referrer is an absolute http(s) URL with a host, or an
 * app-relative path short enough to store.
 *
 * `//host/path` and `/\host/path` are not app-relative: a browser resolves both
 * against the current scheme, so accepting them would file another origin as
 * internal traffic (Go `isReferrer`).
 */
function isWireReferrer(referrer: string): boolean {
  if (utf8Bytes(referrer) > MAX_REFERRER_LEN) {
    return false;
  }
  if (referrer.startsWith('/')) {
    return referrer.length < 2 || (referrer[1] !== '/' && referrer[1] !== '\\');
  }
  let parsed: URL;
  try {
    parsed = new URL(referrer);
  } catch {
    return false;
  }
  return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && parsed.host !== '';
}

/** Reads `page.utm`, in sorted key order so two runs report the same reasons. */
function readUtm(value: unknown, reasons: DropReason[]): UtmFields {
  if (value === undefined || value === null) {
    return {};
  }
  if (!isPlainObject(value)) {
    reasons.push('attribute_kind');
    return {};
  }
  const utm: UtmFields = {};
  for (const key of Object.keys(value).sort()) {
    const entry = value[key];
    if (!isUtmKey(key)) {
      reasons.push('invalid_utm');
    } else if (typeof entry !== 'string') {
      reasons.push('attribute_kind');
    } else if (entry === '' || utf8Bytes(entry) > MAX_UTM_LEN) {
      reasons.push('invalid_utm');
    } else {
      utm[key] = entry;
    }
  }
  return utm;
}

/** Reads the typed action properties (Go `validateProps`). */
function readProps(value: unknown, reasons: DropReason[]): Record<string, string | number | boolean> | undefined {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (!isPlainObject(value)) {
    reasons.push('attribute_kind');
    return undefined;
  }
  const keys = Object.keys(value).sort();
  if (keys.length === 0) {
    return {};
  }
  if (keys.length > MAX_PROP_KEYS) {
    reasons.push('props_too_many');
  }
  const props: Record<string, string | number | boolean> = {};
  for (const key of keys) {
    if (!PROP_KEY_RE.test(key)) {
      reasons.push('invalid_prop_key');
    }
    const entry = value[key];
    if (isPropValue(entry)) {
      props[key] = entry;
    } else {
      reasons.push('invalid_prop_value');
    }
  }
  return props;
}

/** Reports whether a property value is one of the three accepted scalar kinds. */
function isPropValue(value: unknown): value is string | number | boolean {
  if (typeof value === 'string') {
    return utf8Bytes(value) <= MAX_PROP_STRING_LEN;
  }
  if (typeof value === 'boolean') {
    return true;
  }
  return typeof value === 'number' && Number.isFinite(value);
}

/** Assembles the accepted event from a draft that produced no reason. */
function buildEvent(draft: EventDraft, reasons: DropReason[]): SanitizedEvent | undefined {
  const { eventId, clientTs, sessionId, seq, engagementMs, viewportClass, language } = draft;
  if (clientTs === null) {
    reasons.push('missing_attribute');
    return undefined;
  }
  const common = { eventId, clientTs, seq, sessionId, engagementMs, viewportClass, language };
  if (draft.name === EVENT_PAGE_VIEW && draft.page !== undefined) {
    const { path, route, referrer, utm } = draft.page;
    return { kind: 'page_view', ...common, path, route, referrer, utm };
  }
  if (draft.name === EVENT_ACTION) {
    return { kind: 'action', ...common, action: draft.action, props: draft.props ?? {} };
  }
  reasons.push('unknown_event');
  return undefined;
}

/** Reads a required string member: absent, null, and empty are all missing. */
function readRequiredString(raw: Record<string, unknown>, key: string, reasons: DropReason[]): string {
  const value = raw[key];
  if (value === undefined || value === null || value === '') {
    reasons.push('missing_attribute');
    return '';
  }
  if (typeof value !== 'string') {
    reasons.push('attribute_kind');
    return '';
  }
  return value;
}

/** Reads an optional string member; an empty string reads as absent, as in Go. */
function readOptionalString(raw: Record<string, unknown>, key: string, reasons: DropReason[]): string | null {
  const value = raw[key];
  if (value === undefined || value === null || value === '') {
    return null;
  }
  if (typeof value !== 'string') {
    reasons.push('attribute_kind');
    return null;
  }
  return value;
}

/**
 * Reads an optional integer member.
 *
 * `Number.isInteger`, not `typeof value === 'number'`: Go decodes `seq` and
 * `engagementMs` into `int`, so `1.5` is a decode failure there and surfaces as
 * `attribute_kind`. A `typeof` check would accept it and store a fractional
 * sequence number the Go twin rejects.
 */
function readInteger(value: unknown, reasons: DropReason[]): number | null {
  if (value === undefined || value === null) {
    return null;
  }
  if (!Number.isInteger(value)) {
    reasons.push('attribute_kind');
    return null;
  }
  return value as number;
}

/** Parses a contract timestamp, returning null when it is absent or malformed. */
function parseTimestamp(value: unknown): Date | null {
  if (typeof value !== 'string' || !TIMESTAMP_RE.test(value)) {
    return null;
  }
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? null : parsed;
}

/** Reports whether an object carries a key the contract does not define. */
function hasUnknownKeys(value: Record<string, unknown>, allowed: ReadonlySet<string>): boolean {
  return Object.keys(value).some((key) => !allowed.has(key));
}

/** Reports whether a value is a JSON object (not an array, not null). */
function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
