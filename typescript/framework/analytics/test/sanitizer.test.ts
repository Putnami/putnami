import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { type SanitizeResult, sanitizeBatch } from '../src/server/sanitize/sanitizer';
import {
  type DropReason,
  MAX_ENGAGEMENT_MS,
  MAX_PATH_LEN,
  MAX_PROP_STRING_LEN,
  MAX_REFERRER_LEN,
  MAX_SEQ,
  MAX_UTM_LEN,
  utf8Bytes,
} from '../src/server/sanitize/vocabulary';

const FEATURE = 'typescript/web-analytics-collection';
const UNDECLARED = 'undeclared-actions-are-dropped';

const EVENT_ID = '0199116c-8f00-7a1b-8c2d-3e4f5a6b7c8d';
const SESSION_ID = '0199116c-8e00-7a1b-8c2d-3e4f5a6b7c00';
const SENT_AT = '2026-09-02T10:00:00.000Z';
const CLIENT_TS = '2026-09-02T09:59:59.500Z';
const DECLARED = new Set(['signup_click', 'search']);

function pageView(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    eventId: EVENT_ID,
    name: 'page_view',
    clientTs: CLIENT_TS,
    seq: 0,
    sessionId: SESSION_ID,
    page: { path: '/' },
    ...overrides,
  };
}

function actionEvent(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    eventId: EVENT_ID,
    name: 'action',
    clientTs: CLIENT_TS,
    seq: 1,
    sessionId: SESSION_ID,
    action: 'signup_click',
    ...overrides,
  };
}

function batch(events: unknown, envelope: Record<string, unknown> = {}): Record<string, unknown> {
  return { protocolVersion: 1, sentAt: SENT_AT, events, ...envelope };
}

function reasonsOf(result: SanitizeResult): DropReason[] {
  return result.dropped.map((drop) => drop.reason);
}

/** Sanitizes a batch of one event and returns the reasons it produced. */
function dropReasons(event: unknown): DropReason[] {
  return reasonsOf(sanitizeBatch(batch([event]), DECLARED));
}

describe('sanitizeBatch: accepted events', () => {
  it('accepts a minimal page view', () => {
    const result = sanitizeBatch(batch([pageView()]), DECLARED);

    expect(result.dropped).toEqual([]);
    expect(result.sentAt).toEqual(new Date(SENT_AT));
    expect(result.events).toEqual([
      {
        kind: 'page_view',
        eventId: EVENT_ID,
        clientTs: new Date(CLIENT_TS),
        seq: 0,
        sessionId: SESSION_ID,
        engagementMs: null,
        viewportClass: null,
        language: null,
        path: '/',
        route: null,
        referrer: null,
        utm: {},
      },
    ]);
  });

  it('accepts every optional member of a page view', () => {
    const event = pageView({
      engagementMs: 12_500,
      viewportClass: 'lg',
      language: 'fr-FR',
      page: {
        path: '/tasks/42',
        route: '/tasks/[id]',
        referrer: 'https://www.google.com/search?q=putnami',
        utm: { source: 'google', medium: 'cpc', campaign: 'launch-2026', content: 'hero', term: 'putnami' },
      },
    });

    const result = sanitizeBatch(batch([event]), DECLARED);

    expect(result.dropped).toEqual([]);
    expect(result.events[0]).toMatchObject({
      kind: 'page_view',
      engagementMs: 12_500,
      viewportClass: 'lg',
      language: 'fr-FR',
      path: '/tasks/42',
      route: '/tasks/[id]',
      utm: { source: 'google', medium: 'cpc', campaign: 'launch-2026', content: 'hero', term: 'putnami' },
    });
  });

  it('accepts a declared action with typed props', () => {
    const result = sanitizeBatch(batch([actionEvent({ props: { plan: 'pro', seats: 3, trial: true } })]), DECLARED);

    expect(result.dropped).toEqual([]);
    expect(result.events[0]).toMatchObject({
      kind: 'action',
      action: 'signup_click',
      props: { plan: 'pro', seats: 3, trial: true },
    });
  });

  it('keeps the good events of a partially valid batch', () => {
    const result = sanitizeBatch(batch([pageView(), pageView({ eventId: 'not-a-uuid' })]), DECLARED);

    expect(result.events).toHaveLength(1);
    expect(result.dropped).toEqual([{ reason: 'invalid_event_id', index: 1 }]);
  });

  it('accepts the size ceiling and rejects one event more', () => {
    const fifty = Array.from({ length: 50 }, (_unused, index) => pageView({ seq: index }));

    expect(sanitizeBatch(batch(fifty), DECLARED).events).toHaveLength(50);
    expect(reasonsOf(sanitizeBatch(batch([...fifty, pageView()]), DECLARED))).toEqual(['batch_too_large']);
  });
});

describe('sanitizeBatch: envelope rejections', () => {
  it('drops the whole batch for an undefined top-level key', () => {
    const result = sanitizeBatch(batch([pageView()], { title: 'x' }), DECLARED);

    expect(result).toEqual({ sentAt: null, events: [], dropped: [{ reason: 'unknown_attribute', index: null }] });
  });

  it('drops anything that is not a JSON object', () => {
    for (const input of [null, undefined, 42, 'batch', [pageView()], true]) {
      expect(reasonsOf(sanitizeBatch(input, DECLARED))).toEqual(['unknown_attribute']);
    }
  });

  it('reports the version, the timestamp, and the size against the whole batch', () => {
    expect(reasonsOf(sanitizeBatch(batch([pageView()], { protocolVersion: 2 }), DECLARED))).toEqual([
      'invalid_version',
    ]);
    expect(reasonsOf(sanitizeBatch(batch([pageView()], { sentAt: '2026-09-02' }), DECLARED))).toEqual([
      'invalid_timestamp',
    ]);
    expect(reasonsOf(sanitizeBatch(batch([]), DECLARED))).toEqual(['batch_too_large']);
    expect(reasonsOf(sanitizeBatch(batch({}), DECLARED))).toEqual(['batch_too_large']);
    expect(sanitizeBatch(batch([]), DECLARED).dropped[0]?.index).toBeNull();
  });

  it('reports every envelope violation at once', () => {
    const result = sanitizeBatch({ protocolVersion: 9, sentAt: 'nope', events: [] }, DECLARED);

    expect(reasonsOf(result)).toEqual(['invalid_version', 'invalid_timestamp', 'batch_too_large']);
  });
});

describe('sanitizeBatch: one case per event drop reason', () => {
  it('invalid_event_id, for both the event and the session', () => {
    expect(dropReasons(pageView({ eventId: '0199116c-8f00-4a1b-8c2d-3e4f5a6b7c8d' }))).toEqual(['invalid_event_id']);
    expect(dropReasons(pageView({ sessionId: 'not-a-uuid' }))).toEqual(['invalid_event_id']);
    expect(dropReasons(pageView({ eventId: EVENT_ID.toUpperCase() }))).toEqual(['invalid_event_id']);
  });

  it('unknown_event, including the server-only form_submit', () => {
    expect(dropReasons(pageView({ name: 'form_submit' }))).toEqual(['unknown_event']);
    expect(dropReasons(pageView({ name: 'click' }))).toEqual(['unknown_event']);
  });

  it('unknown_attribute, for a field the contract does not define', () => {
    expect(dropReasons(pageView({ title: 'Home' }))).toEqual(['unknown_attribute']);
    expect(dropReasons(pageView({ page: { path: '/', query: 'q=1' } }))).toEqual(['unknown_attribute']);
    expect(dropReasons(actionEvent({ page: { path: '/' } }))).toEqual(['unknown_attribute']);
  });

  it('missing_attribute, for an absent required field', () => {
    expect(dropReasons(pageView({ eventId: undefined }))).toEqual(['missing_attribute']);
    expect(dropReasons(pageView({ clientTs: '' }))).toEqual(['missing_attribute']);
    expect(dropReasons(pageView({ sessionId: null }))).toEqual(['missing_attribute']);
    expect(dropReasons(pageView({ page: undefined }))).toEqual(['missing_attribute']);
    expect(dropReasons(actionEvent({ action: undefined }))).toEqual(['missing_attribute']);
  });

  it('attribute_kind, for a member carrying the wrong JSON type', () => {
    expect(dropReasons(pageView({ eventId: 42 }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ page: 'home' }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ page: { path: 42 } }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ viewportClass: 3 }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ page: { path: '/', utm: { source: 42 } } }))).toEqual(['attribute_kind']);
    expect(dropReasons(actionEvent({ props: ['plan'] }))).toEqual(['attribute_kind']);
    expect(dropReasons(42)).toEqual(['attribute_kind']);
  });

  it('invalid_value, for a scalar outside its bounds', () => {
    expect(dropReasons(pageView({ engagementMs: -1 }))).toEqual(['invalid_value']);
    expect(dropReasons(pageView({ engagementMs: MAX_ENGAGEMENT_MS + 1 }))).toEqual(['invalid_value']);
    expect(dropReasons(pageView({ seq: MAX_SEQ + 1 }))).toEqual(['invalid_value']);
    expect(dropReasons(pageView({ seq: -1 }))).toEqual(['invalid_value']);
    expect(dropReasons(pageView({ viewportClass: 'xxl' }))).toEqual(['invalid_value']);
    expect(dropReasons(pageView({ language: 'français' }))).toEqual(['invalid_value']);
  });

  it('invalid_path, invalid_route, and invalid_referrer', () => {
    expect(dropReasons(pageView({ page: { path: '/a?b=1' } }))).toEqual(['invalid_path']);
    expect(dropReasons(pageView({ page: { path: '/a#b' } }))).toEqual(['invalid_path']);
    expect(dropReasons(pageView({ page: { path: 'tasks' } }))).toEqual(['invalid_path']);
    expect(dropReasons(pageView({ page: {} }))).toEqual(['invalid_path']);
    expect(dropReasons(pageView({ page: { path: '/', route: '/tasks/:id' } }))).toEqual(['invalid_route']);
    expect(dropReasons(pageView({ page: { path: '/', referrer: 'javascript:alert(1)' } }))).toEqual([
      'invalid_referrer',
    ]);
    // A browser resolves `//host/path` against the current scheme, so it is
    // another origin wearing a path's clothes.
    expect(dropReasons(pageView({ page: { path: '/', referrer: '//evil.example.com/x' } }))).toEqual([
      'invalid_referrer',
    ]);
  });

  it('invalid_timestamp, for a malformed clientTs', () => {
    expect(dropReasons(pageView({ clientTs: '2026-09-02' }))).toEqual(['invalid_timestamp']);
    expect(dropReasons(pageView({ clientTs: '2026-09-02T09:59:59Z' }))).toEqual(['invalid_timestamp']);
    expect(dropReasons(pageView({ clientTs: 42 }))).toEqual(['attribute_kind']);
  });

  it('attribute_kind, for a page member carrying the wrong type', () => {
    expect(dropReasons(pageView({ page: { path: '/', route: 42 } }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ page: { path: '/', referrer: 42 } }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ page: { path: '/', utm: 'source=google' } }))).toEqual(['attribute_kind']);
  });

  it('invalid_referrer, for an unparseable or over-long value', () => {
    const long = `https://long.example.com/${'a'.repeat(MAX_REFERRER_LEN)}`;
    expect(utf8Bytes(long)).toBeGreaterThan(MAX_REFERRER_LEN);

    expect(dropReasons(pageView({ page: { path: '/', referrer: 'not a url' } }))).toEqual(['invalid_referrer']);
    expect(dropReasons(pageView({ page: { path: '/', referrer: long } }))).toEqual(['invalid_referrer']);
    expect(dropReasons(pageView({ page: { path: '/', referrer: '/dashboard' } }))).toEqual([]);
  });

  it('accepts an empty props object on an action', () => {
    const result = sanitizeBatch(batch([actionEvent({ props: {} })]), DECLARED);

    expect(result.dropped).toEqual([]);
    expect(result.events[0]).toMatchObject({ kind: 'action', props: {} });
  });

  it('invalid_utm, for an unknown key or an empty value', () => {
    expect(dropReasons(pageView({ page: { path: '/', utm: { campaign_id: 'x' } } }))).toEqual(['invalid_utm']);
    expect(dropReasons(pageView({ page: { path: '/', utm: { source: '' } } }))).toEqual(['invalid_utm']);
  });

  it('invalid_action and unknown_action', () => {
    expect(dropReasons(actionEvent({ action: 'Sign Up' }))).toEqual(['invalid_action']);
    expect(dropReasons(actionEvent({ action: `a${'b'.repeat(64)}` }))).toEqual(['invalid_action']);
  });

  it('props_too_many, invalid_prop_key, and invalid_prop_value', () => {
    const many = Object.fromEntries(Array.from({ length: 21 }, (_unused, index) => [`key_${index}`, index]));

    expect(dropReasons(actionEvent({ props: many }))).toEqual(['props_too_many']);
    expect(dropReasons(actionEvent({ props: { Plan: 'pro' } }))).toEqual(['invalid_prop_key']);
    expect(dropReasons(actionEvent({ props: { plan: { nested: true } } }))).toEqual(['invalid_prop_value']);
    expect(dropReasons(actionEvent({ props: { plan: Number.POSITIVE_INFINITY } }))).toEqual(['invalid_prop_value']);
    expect(dropReasons(actionEvent({ props: { plan: null } }))).toEqual(['invalid_prop_value']);
  });

  it('reports both reasons when a member is misplaced and malformed', () => {
    // A page view carrying props is misplaced, and the props are still checked:
    // this is the twin of the Go fixtures that report two codes.
    expect(dropReasons(pageView({ props: { Plan: 'pro' } })).sort()).toEqual(['invalid_prop_key', 'unknown_attribute']);
    expect(dropReasons(pageView({ action: 'Sign Up' })).sort()).toEqual(['invalid_action', 'unknown_attribute']);
  });

  it('never throws, whatever the document is', () => {
    const hostile = {
      protocolVersion: 1,
      sentAt: SENT_AT,
      get events(): unknown {
        throw new Error('boom');
      },
    };

    expect(sanitizeBatch(hostile, DECLARED)).toEqual({
      sentAt: null,
      events: [],
      dropped: [{ reason: 'attribute_kind', index: null }],
    });
  });
});

describe('sanitizeBatch: the two rules JavaScript gets wrong', () => {
  specTest(
    'rejects an undeclared action name',
    { feature: FEATURE, requirement: UNDECLARED, check: 'an-undeclared-action-is-dropped-and-counted' },
    () => {
      const result = sanitizeBatch(batch([actionEvent({ action: 'exfiltrate' })]), DECLARED);

      expect(result.events).toEqual([]);
      expect(result.dropped).toEqual([{ reason: 'unknown_action', index: 0 }]);
      expect(sanitizeBatch(batch([actionEvent({ action: 'search' })]), DECLARED).dropped).toEqual([]);
      // An undeclared name never reaches the database even when it is well
      // formed: only the application decides what its vocabulary is.
      expect(sanitizeBatch(batch([actionEvent()]), new Set()).dropped).toEqual([
        { reason: 'unknown_action', index: 0 },
      ]);
    },
  );

  specTest(
    'rejects a value beyond a byte bound instead of truncating it',
    { feature: FEATURE, requirement: UNDECLARED, check: 'props-outside-bounds-reject-the-event' },
    () => {
      // Every bound of the contract counts UTF-8 bytes, as Go's len() does. Each
      // value below is under its bound in UTF-16 units — the guard asserts it —
      // and over it in bytes, so a sanitizer using `.length` would accept all
      // three and store documents the Go validator rejects.
      const path = `/${'я'.repeat(300)}`;
      expect(path.length).toBeLessThan(MAX_PATH_LEN);
      expect(utf8Bytes(path)).toBeGreaterThan(MAX_PATH_LEN);
      expect(dropReasons(pageView({ page: { path } }))).toEqual(['invalid_path']);

      const campaign = 'я'.repeat(100);
      expect(campaign.length).toBeLessThan(MAX_UTM_LEN);
      expect(utf8Bytes(campaign)).toBeGreaterThan(MAX_UTM_LEN);
      expect(dropReasons(pageView({ page: { path: '/', utm: { campaign } } }))).toEqual(['invalid_utm']);

      const plan = 'я'.repeat(200);
      expect(plan.length).toBeLessThan(MAX_PROP_STRING_LEN);
      expect(utf8Bytes(plan)).toBeGreaterThan(MAX_PROP_STRING_LEN);
      expect(dropReasons(actionEvent({ props: { plan } }))).toEqual(['invalid_prop_value']);

      // The same values one byte inside the bound are accepted.
      expect(dropReasons(pageView({ page: { path: `/${'я'.repeat(255)}` } }))).toEqual([]);
      expect(dropReasons(actionEvent({ props: { plan: 'я'.repeat(128) } }))).toEqual([]);
    },
  );

  it('requires whole numbers for seq and engagementMs', () => {
    // Go decodes both into an int, so 1.5 is a decode failure there and
    // surfaces as attribute_kind. `typeof value === 'number'` would accept it.
    expect(dropReasons(pageView({ seq: 1.5 }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ engagementMs: 1.5 }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ seq: '1' }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ seq: Number.NaN }))).toEqual(['attribute_kind']);
    expect(dropReasons(pageView({ seq: 1 }))).toEqual([]);
    expect(dropReasons(pageView({ engagementMs: 0 }))).toEqual([]);
  });
});
