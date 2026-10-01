import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { DIMENSIONS } from '../src/server/sanitize/vocabulary';
import { NONE_KEY, OVERFLOW_KEY, planFold, referrerHostOf } from '../src/server/sink/fold';
import { actionRow, formSubmitRow, pageViewRow, resultFor } from './utils/fixtures';

const FEATURE = 'typescript/web-analytics-collection';
const REQUIREMENT = 'a-retried-event-is-stored-once';
const PREFOLD = 'sessions-prefold-duplicate-ids-before-upsert';

const SESSION = '01920000-0000-7000-8000-0000000000aa';
const NO_OVERFLOW = new Set<string>();

describe('planFold', () => {
  specTest(
    'folds two page views of one session into a single upsert row',
    { feature: FEATURE, requirement: REQUIREMENT, check: PREFOLD },
    () => {
      const first = pageViewRow({
        eventId: '01920000-0000-7000-8000-00000000000a',
        route: '/',
        ts: new Date('2026-09-02T10:00:00.000Z'),
      });
      const second = pageViewRow({
        eventId: '01920000-0000-7000-8000-00000000000b',
        route: '/pricing',
        ts: new Date('2026-09-02T10:04:00.000Z'),
      });
      const rows = [second, first];

      const plan = planFold(rows, [resultFor(second), resultFor(first)], NO_OVERFLOW);

      // One row per (day, session_id): `ON CONFLICT ... DO UPDATE` cannot touch
      // the same row twice in one statement, so a second entry would abort S4.
      expect(plan.sessions).toHaveLength(1);
      expect(plan.sessions[0]).toMatchObject({
        day: '2026-09-02',
        sessionId: SESSION,
        pageViews: 2,
        firstRoute: '/',
        lastRoute: '/pricing',
        startedAt: first.ts,
        endedAt: second.ts,
      });
      expect(plan.touchedSessions).toEqual([{ day: '2026-09-02', sessionId: SESSION }]);
    },
  );

  it('keeps an enriched row out of every aggregate but the engagement recompute', () => {
    const inserted = pageViewRow({ eventId: '01920000-0000-7000-8000-00000000000c' });
    const enriched = pageViewRow({
      eventId: '01920000-0000-7000-8000-00000000000d',
      sessionId: '01920000-0000-7000-8000-0000000000bb',
      visitorId: 'visitor-bbbbbbbbbbbbbb',
    });

    const plan = planFold([inserted, enriched], [resultFor(inserted), resultFor(enriched, false)], NO_OVERFLOW);

    expect(plan.visitors).toEqual([{ day: '2026-09-02', visitorId: 'visitor-aaaaaaaaaaaaaa' }]);
    expect(plan.sessions.map((session) => session.sessionId)).toEqual([SESSION]);
    expect(plan.counters.every((counter) => counter.count === 1)).toBe(true);
    // The enriched row still owns a session whose engagement total changed.
    expect(plan.touchedSessions.map((session) => session.sessionId)).toEqual([
      '01920000-0000-7000-8000-0000000000aa',
      '01920000-0000-7000-8000-0000000000bb',
    ]);
  });

  it('drops a result whose row is not in the batch', () => {
    const row = pageViewRow();
    const stray = resultFor(pageViewRow({ eventId: '01920000-0000-7000-8000-0000000000ff' }));

    const plan = planFold([row], [resultFor(row), stray], NO_OVERFLOW);

    expect(plan.visitors).toHaveLength(1);
    expect(plan.counters.filter((counter) => counter.dimension === 'route')).toHaveLength(1);
  });

  it('folds a missing campaign value under __none__', () => {
    const row = pageViewRow({ utmSource: null, utmMedium: null, utmCampaign: null, country: null, language: null });

    const plan = planFold([row], [resultFor(row)], NO_OVERFLOW);

    const keys = new Map(plan.counters.map((counter) => [counter.dimension, counter.key]));
    expect(keys.get('utm_source')).toBe(NONE_KEY);
    expect(keys.get('utm_medium')).toBe(NONE_KEY);
    expect(keys.get('utm_campaign')).toBe(NONE_KEY);
    expect(keys.get('country')).toBe(NONE_KEY);
    expect(keys.get('language')).toBe(NONE_KEY);
  });

  it('emits exactly the closed dimension set of the protocol', () => {
    const page = pageViewRow();
    const action = actionRow();
    const form = formSubmitRow();
    const rows = [page, action, form];

    const plan = planFold(
      rows,
      rows.map((row) => resultFor(row)),
      NO_OVERFLOW,
    );

    const emitted = [...new Set(plan.counters.map((counter) => counter.dimension))].sort();
    expect(emitted).toEqual([...DIMENSIONS].sort());
    const byDimension = new Map(plan.counters.map((counter) => [counter.dimension, counter.key]));
    expect(byDimension.get('action')).toBe('signup_started');
    expect(byDimension.get('form_submit')).toBe('/signup|ok');
    expect(byDimension.get('referrer_host')).toBe('google.com');
  });

  it('aggregates a repeated key once and emits sorted parameters', () => {
    const first = pageViewRow({ eventId: '01920000-0000-7000-8000-000000000011' });
    const second = pageViewRow({ eventId: '01920000-0000-7000-8000-000000000012' });

    const plan = planFold([first, second], [resultFor(first), resultFor(second)], NO_OVERFLOW);

    const route = plan.counters.filter((counter) => counter.dimension === 'route');
    expect(route).toEqual([{ day: '2026-09-02', dimension: 'route', key: '/docs/[slug]', count: 2 }]);
    const keys = plan.counters.map((counter) => `${counter.day}|${counter.dimension}|${counter.key}`);
    expect(keys).toEqual([...keys].sort());
  });

  it('folds every path under __overflow__ once the daily cap is reached', () => {
    const row = pageViewRow();

    const plan = planFold([row], [resultFor(row)], new Set([row.day]));

    expect(plan.counters.find((counter) => counter.dimension === 'path')?.key).toBe(OVERFLOW_KEY);
  });

  it('counts a page view with no session and no path', () => {
    const row = pageViewRow({ sessionId: null, path: null, referrer: null });

    const plan = planFold([row], [resultFor(row)], NO_OVERFLOW);

    expect(plan.sessions).toHaveLength(0);
    expect(plan.touchedSessions).toHaveLength(0);
    expect(plan.counters.find((counter) => counter.dimension === 'path')?.key).toBe(NONE_KEY);
  });

  it('folds an action with no name and a form submit with no outcome under __none__', () => {
    const action = actionRow({ actionName: null });
    const form = formSubmitRow({ outcome: null });
    const rows = [action, form];

    const plan = planFold(
      rows,
      rows.map((row) => resultFor(row)),
      NO_OVERFLOW,
    );

    const byDimension = new Map(plan.counters.map((counter) => [counter.dimension, counter.key]));
    expect(byDimension.get('action')).toBe(NONE_KEY);
    expect(byDimension.get('form_submit')).toBe(`/signup|${NONE_KEY}`);
  });

  it('normalizes a day the driver decoded as a Date before binding it again', () => {
    const row = pageViewRow();
    const result = { ...resultFor(row), day: new Date(`${row.day}T00:00:00.000Z`) };

    const plan = planFold([row], [result], NO_OVERFLOW);

    // A `date` column comes back from the driver as a JS `Date`. Handing that
    // value straight to the next statement's `$1::date[]` makes the driver
    // declare the parameter `timestamptz`, and Postgres refuses the cast.
    expect(plan.touchedSessions).toEqual([{ day: row.day, sessionId: row.sessionId as string }]);
  });

  it('returns nothing for a batch the upsert reported nothing for', () => {
    expect(planFold([], [], NO_OVERFLOW)).toEqual({ counters: [], visitors: [], sessions: [], touchedSessions: [] });
  });
});

describe('referrerHostOf', () => {
  it('strips the www prefix of an external host', () => {
    expect(referrerHostOf('https://www.example.com/blog')).toBe('example.com');
  });

  it('reports __none__ for an absent, internal, or unparsable referrer', () => {
    expect(referrerHostOf(null)).toBe(NONE_KEY);
    expect(referrerHostOf('')).toBe(NONE_KEY);
    // Internal traffic is stored app-relative and carries no host to count;
    // `referrer_type` is what keeps the visit classified.
    expect(referrerHostOf('/pricing')).toBe(NONE_KEY);
    expect(referrerHostOf('not a url')).toBe(NONE_KEY);
  });
});
