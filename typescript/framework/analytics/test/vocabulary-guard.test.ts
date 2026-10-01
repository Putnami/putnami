import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { sanitizeBatch } from '../src/server/sanitize/sanitizer';
import {
  ACTION_NAME_RE,
  BROWSERS,
  DEVICE_TYPES,
  DIMENSIONS,
  DROP_REASONS,
  EVENT_ACTION,
  EVENT_COLUMNS,
  EVENT_FORM_SUBMIT,
  EVENT_PAGE_VIEW,
  LANGUAGE_RE,
  MAX_ACTION_NAME_LEN,
  MAX_BODY_BYTES,
  MAX_ENGAGEMENT_MS,
  MAX_EVENTS,
  MAX_PATH_LEN,
  MAX_PROP_KEY_LEN,
  MAX_PROP_KEYS,
  MAX_PROP_STRING_LEN,
  MAX_REFERRER_LEN,
  MAX_ROUTE_LEN,
  MAX_SEQ,
  MAX_UTM_LEN,
  OPERATING_SYSTEMS,
  OUTCOMES,
  PROP_KEY_RE,
  PROTOCOL_VERSION,
  REFERRER_TYPES,
  ROUTE_RE,
  SOURCES,
  TIMESTAMP_RE,
  UTM_KEYS,
  UUID_V7_RE,
  VIEWPORT_CLASSES,
  VISITOR_KINDS,
} from '../src/server/sanitize/vocabulary';

// protocols/analytics/analytics.go is the contract; this file is its only copy
// in TypeScript. The guard reads the Go source as text so a drift in either
// direction fails here rather than in production, where the two runtimes would
// simply disagree about what a valid document is.
const GO_SOURCE = readFileSync(join(__dirname, '../../../../protocols/analytics/analytics.go'), 'utf8');
const GO_LINES = GO_SOURCE.split('\n');

/**
 * Every token the Go contract names: the JSON tags of its types plus every
 * quoted literal of its enums, event names, and error codes.
 */
const PROTOCOL_TOKENS = new Set([
  ...Array.from(GO_SOURCE.matchAll(/json:"([A-Za-z0-9_]+)/g), (match) => match[1] ?? ''),
  ...Array.from(GO_SOURCE.matchAll(/"([^"]*)"/g), (match) => match[1] ?? ''),
]);

function goDeclaration(name: string): string {
  // A declaration is either inside a `const (` block or a single `const x = y`.
  const line = GO_LINES.find((candidate) => candidate.trim().replace('const ', '').startsWith(`${name} = `));
  if (line === undefined) {
    throw new Error(`analytics.go declares no ${name}`);
  }
  return line;
}

/** Reads a Go `[]string{...}` literal, which may wrap over several lines. */
function goStrings(name: string): string[] {
  const start = GO_LINES.findIndex((line) => line.trim().startsWith(`${name} = []string{`));
  if (start === -1) {
    throw new Error(`analytics.go declares no ${name}`);
  }
  let block = '';
  for (let index = start; index < GO_LINES.length; index++) {
    block += GO_LINES[index];
    if (GO_LINES[index]?.includes('}')) {
      break;
    }
  }
  return Array.from(block.matchAll(/"([^"]*)"/g), (match) => match[1] ?? '');
}

function goInt(name: string): number {
  const match = /=\s*([0-9_]+)/.exec(goDeclaration(name));
  return Number.parseInt((match?.[1] ?? '').replaceAll('_', ''), 10);
}

function goString(name: string): string {
  return /"([^"]*)"/.exec(goDeclaration(name))?.[1] ?? '';
}

function goRegex(name: string): string {
  return /`([^`]*)`/.exec(goDeclaration(name))?.[1] ?? '';
}

/**
 * Drops the escapes JavaScript requires and Go does not: `/` must be escaped in
 * a regex literal, and Biome normalises `\-` inside a character class. Every
 * other character is compared as written.
 */
function normalizeRegex(source: string): string {
  return source.replaceAll('\\/', '/').replaceAll('\\-', '-');
}

describe('vocabulary parity with protocols/analytics/analytics.go', () => {
  it('copies every closed enum, member for member and in order', () => {
    expect(VIEWPORT_CLASSES).toEqual(goStrings('ViewportClasses'));
    expect(REFERRER_TYPES).toEqual(goStrings('ReferrerTypes'));
    expect(DEVICE_TYPES).toEqual(goStrings('DeviceTypes'));
    expect(BROWSERS).toEqual(goStrings('Browsers'));
    expect(OPERATING_SYSTEMS).toEqual(goStrings('OperatingSystems'));
    expect(SOURCES).toEqual(goStrings('Sources'));
    expect(VISITOR_KINDS).toEqual(goStrings('VisitorKinds'));
    expect(OUTCOMES).toEqual(goStrings('Outcomes'));
    expect(UTM_KEYS).toEqual(goStrings('UTMKeys'));
    expect(DIMENSIONS).toEqual(goStrings('Dimensions'));
  });

  it('copies every bound', () => {
    expect(PROTOCOL_VERSION).toBe(goInt('ProtocolVersion'));
    expect(MAX_EVENTS).toBe(goInt('MaxEvents'));
    expect(MAX_BODY_BYTES).toBe(goInt('MaxBodyBytes'));
    expect(MAX_SEQ).toBe(goInt('MaxSeq'));
    expect(MAX_ENGAGEMENT_MS).toBe(goInt('MaxEngagementMs'));
    expect(MAX_PATH_LEN).toBe(goInt('MaxPathLen'));
    expect(MAX_ROUTE_LEN).toBe(goInt('MaxRouteLen'));
    expect(MAX_REFERRER_LEN).toBe(goInt('MaxReferrerLen'));
    expect(MAX_UTM_LEN).toBe(goInt('MaxUTMLen'));
    expect(MAX_ACTION_NAME_LEN).toBe(goInt('MaxActionNameLen'));
    expect(MAX_PROP_KEYS).toBe(goInt('MaxPropKeys'));
    expect(MAX_PROP_KEY_LEN).toBe(goInt('MaxPropKeyLen'));
    expect(MAX_PROP_STRING_LEN).toBe(goInt('MaxPropStringLen'));
  });

  it('copies every regular expression character for character', () => {
    expect(normalizeRegex(TIMESTAMP_RE.source)).toBe(normalizeRegex(goRegex('TimestampRe')));
    expect(normalizeRegex(UUID_V7_RE.source)).toBe(normalizeRegex(goRegex('UUIDv7Re')));
    expect(normalizeRegex(ROUTE_RE.source)).toBe(normalizeRegex(goRegex('RouteRe')));
    expect(normalizeRegex(ACTION_NAME_RE.source)).toBe(normalizeRegex(goRegex('ActionNameRe')));
    expect(normalizeRegex(PROP_KEY_RE.source)).toBe(normalizeRegex(goRegex('PropKeyRe')));
    expect(normalizeRegex(LANGUAGE_RE.source)).toBe(normalizeRegex(goRegex('LanguageRe')));
  });

  it('copies the event names', () => {
    expect(EVENT_PAGE_VIEW).toBe(goString('EventPageView'));
    expect(EVENT_ACTION).toBe(goString('EventAction'));
    expect(EVENT_FORM_SUBMIT).toBe(goString('EventFormSubmit'));
  });

  it('counts the Go error codes, and only adds reasons Go cannot know', () => {
    const goCodes = new Set(
      Array.from(GO_SOURCE.matchAll(/"analytics\.([a-z_]+)"/g), (match) => match[1] ?? '').filter(
        (code) => code !== 'parse_error',
      ),
    );
    const serverOnly = ['unknown_action', 'bot', 'rate_limited', 'duplicate', 'overflow'];

    // Every Go code is a drop reason, and every reason that is not a Go code is
    // one only the server can observe: a declaration, a bot, a rate limit, a
    // duplicate, a full write queue.
    expect([...goCodes].sort()).toEqual(DROP_REASONS.filter((reason) => !serverOnly.includes(reason)).sort());
    for (const reason of serverOnly) {
      expect(goCodes.has(reason)).toBe(false);
      expect(DROP_REASONS).toContain(reason);
    }
  });
});

describe('the persisted vocabulary', () => {
  it('emits no attribute the protocol does not define', () => {
    const result = sanitizeBatch(
      {
        protocolVersion: 1,
        sentAt: '2026-09-02T10:00:00.000Z',
        events: [
          {
            eventId: '0199116c-8f00-7a1b-8c2d-3e4f5a6b7c8d',
            name: 'page_view',
            clientTs: '2026-09-02T09:59:59.500Z',
            seq: 0,
            sessionId: '0199116c-8e00-7a1b-8c2d-3e4f5a6b7c00',
            engagementMs: 10,
            viewportClass: 'lg',
            language: 'fr',
            page: { path: '/', route: '/', referrer: '/x', utm: { source: 'google' } },
          },
          {
            eventId: '0199116c-9100-7a1b-ac2d-3e4f5a6b7c02',
            name: 'action',
            clientTs: '2026-09-02T09:59:57.750Z',
            seq: 1,
            sessionId: '0199116c-8e00-7a1b-8c2d-3e4f5a6b7c00',
            action: 'signup_click',
            props: { plan: 'pro' },
          },
        ],
      },
      new Set(['signup_click']),
    );
    expect(result.events).toHaveLength(2);

    const emitted = new Set(result.events.flatMap((event) => Object.keys(event)));
    for (const key of emitted) {
      if (key === 'kind') {
        // `kind` is the TypeScript discriminant; its values are the protocol's
        // own event names, which is what the vocabulary bounds.
        continue;
      }
      expect(PROTOCOL_TOKENS.has(key)).toBe(true);
    }
    for (const event of result.events) {
      expect(PROTOCOL_TOKENS.has(event.kind)).toBe(true);
    }
  });

  it('names no counter dimension or enum value outside the protocol', () => {
    const values = [
      ...DIMENSIONS,
      ...VIEWPORT_CLASSES,
      ...REFERRER_TYPES,
      ...DEVICE_TYPES,
      ...BROWSERS,
      ...OPERATING_SYSTEMS,
      ...SOURCES,
      ...VISITOR_KINDS,
      ...OUTCOMES,
      ...UTM_KEYS,
    ];

    for (const value of values) {
      expect(PROTOCOL_TOKENS.has(value)).toBe(true);
    }
  });

  /**
   * The columns the wire does not name. Each one is derived server-side, and
   * the comment is its provenance: adding a column here is the moment to ask
   * what it holds.
   */
  const DERIVED_COLUMNS: Record<string, string> = {
    received_at: 'the server clock at ingest',
    ts: 'the canonical event time (body §D.8)',
    day: 'the UTC date of ts',
    app: 'the project name',
    env: 'the runtime environment',
    app_version: 'the deployed version',
    visitor_id: 'the daily hash or the cookie id (body §D.1, §D.2)',
    visitor_kind: 'daily or cookie',
    user_id: 'the authenticated principal, when there is one',
    status_code: 'the response status of a server page view',
    render_ms: 'the server render duration',
    action_name: 'the wire action, stored under a suffixed column',
    outcome: 'the form-submission outcome',
    browser_major: 'the classifier output for browserMajor',
  };

  function camelCase(column: string): string {
    const [head, ...rest] = column.split('_');
    return (head ?? '') + rest.map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join('');
  }

  specTest(
    'every persisted column is in the protocol vocabulary',
    {
      feature: 'typescript/web-analytics-collection',
      requirement: 'no-network-address-or-raw-user-agent-is-persisted',
      check: 'every-persisted-column-is-in-the-protocol-vocabulary',
    },
    () => {
      for (const column of EVENT_COLUMNS) {
        const fromProtocol =
          PROTOCOL_TOKENS.has(column) ||
          PROTOCOL_TOKENS.has(camelCase(column)) ||
          (UTM_KEYS as readonly string[]).includes(column.replace('utm_', ''));
        expect(fromProtocol || column in DERIVED_COLUMNS).toBe(true);
      }
      // The derived list is exact: a column that leaves the protocol must be
      // added to it deliberately, not silently.
      for (const column of Object.keys(DERIVED_COLUMNS)) {
        expect(EVENT_COLUMNS).toContain(column);
      }
    },
  );

  it('has no column that could hold personal data', () => {
    // Invariant F.1: no IP address, raw User-Agent, query string, page title,
    // form field, token, or e-mail is ever written. What the schema has no
    // column for cannot be stored by accident.
    for (const forbidden of ['ip', 'userAgent', 'user_agent', 'title', 'query', 'email', 'token']) {
      expect(EVENT_COLUMNS).not.toContain(forbidden);
    }
    expect(EVENT_COLUMNS.filter((column) => column.includes('agent') || column.includes('email'))).toEqual([]);
  });
});
