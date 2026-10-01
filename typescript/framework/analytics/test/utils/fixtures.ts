import type { AnalyticsConfigValues } from '../../src/server/analytics.config';
import type { EventRow, RawUpsertResult } from '../../src/server/sink/fold';

/** A fully resolved analytics configuration, with the shipped defaults. */
export function testConfig(overrides: Partial<AnalyticsConfigValues> = {}): AnalyticsConfigValues {
  return {
    enabled: true,
    mode: 'cookieless',
    datasource: 'analytics',
    serverPageViews: true,
    respectGpc: true,
    respectDnt: true,
    retentionRawDays: 90,
    retentionAggregateDays: 760,
    retentionMode: 'sweep',
    maxPathKeysPerDay: 2000,
    rateLimitPerMinute: 120,
    flushIntervalMs: 5000,
    flushWaitMs: 1000,
    queueCapacity: 5000,
    flushBatch: 200,
    flushDeadlineMs: 5000,
    cookieName: '_pa',
    cookieMaxAgeDays: 390,
    ...overrides,
  } as AnalyticsConfigValues;
}

/** A page-view row with every column filled, so a fold covers every dimension. */
export function pageViewRow(overrides: Partial<EventRow> = {}): EventRow {
  return {
    eventId: '01920000-0000-7000-8000-000000000001',
    ts: new Date('2026-09-02T10:00:00.000Z'),
    day: '2026-09-02',
    name: 'page_view',
    source: 'client',
    app: 'demo',
    env: 'test',
    appVersion: '1.2.3',
    visitorId: 'visitor-aaaaaaaaaaaaaa',
    visitorKind: 'daily',
    sessionId: '01920000-0000-7000-8000-0000000000aa',
    seq: 0,
    userId: null,
    route: '/docs/[slug]',
    path: '/docs/getting-started',
    referrer: 'https://www.google.com/search',
    referrerType: 'search',
    utmSource: 'newsletter',
    utmMedium: 'email',
    utmCampaign: 'launch',
    utmContent: null,
    utmTerm: null,
    statusCode: 200,
    renderMs: 12,
    engagementMs: 0,
    actionName: null,
    outcome: null,
    props: {},
    browser: 'chrome',
    browserMajor: 141,
    os: 'macos',
    deviceType: 'desktop',
    language: 'en-US',
    viewportClass: 'lg',
    country: 'FR',
    ...overrides,
  };
}

/** An action row: no page members, a declared action name, typed properties. */
export function actionRow(overrides: Partial<EventRow> = {}): EventRow {
  return pageViewRow({
    eventId: '01920000-0000-7000-8000-000000000002',
    name: 'action',
    route: '__none__',
    path: null,
    actionName: 'signup_started',
    props: { plan: 'pro' },
    ...overrides,
  });
}

/** A server-side form submission with its outcome. */
export function formSubmitRow(overrides: Partial<EventRow> = {}): EventRow {
  return pageViewRow({
    eventId: '01920000-0000-7000-8000-000000000003',
    name: 'form_submit',
    source: 'server',
    sessionId: null,
    route: '/signup',
    path: null,
    outcome: 'ok',
    ...overrides,
  });
}

/** The `RETURNING` row the upsert would emit for `row`. */
export function resultFor(row: EventRow, inserted = true): RawUpsertResult {
  return {
    eventId: row.eventId,
    inserted,
    name: row.name,
    day: row.day,
    sessionId: row.sessionId,
    visitorId: row.visitorId,
    route: row.route,
    ts: row.ts,
    engagementMs: row.engagementMs,
  };
}
