export { type NavigationDetail, onNavigation, PUTNAMI_NAVIGATION_EVENT } from '@putnami/web';
// The browser hook, exported from the server entry too: a component that calls
// it also renders on the server, and it is a no-op wherever `window` is absent.
export { useTrack } from './client/react/use-track';
export type { TrackFn } from './client/wire';
export * from './server/analytics.config';
export { analytics, AnalyticsPlugin, hasSqlPlugin, NO_SQL_WARNING, startFlushTicker } from './server/analytics.plugin';
export { resolveAnalyticsDatasource } from './server/datasource';
export {
  declareEvents,
  type DeclaredEvents,
  type PropsSchema,
  type PropType,
  type RegisteredEvents,
  registerDeclared,
  validateProps,
} from './server/declare';
export { BOT_TOKENS, isBot } from './server/enrich/bots';
export {
  classifyReferrer,
  extractUtm,
  NO_HOST,
  normalizePath,
  type ReferrerInfo,
  truncateUtf8,
} from './server/enrich/referrer';
export { classifyUserAgent, MAX_USER_AGENT_LEN, type UAInfo } from './server/enrich/user-agent';
export { viewportClassOf } from './server/enrich/viewport';
export { createClientIpResolver } from './server/identity/client-ip';
export {
  ANALYTICS_SET_COOKIE_SLOT,
  type CookieDecision,
  forget,
  mintId,
  optOutSignal,
  parseCookie,
  pushPendingCookie,
  resetConsentWarning,
  resolveIdentifiedVisitor,
  serializeCookie,
  sign,
  SIGNATURE_LENGTH,
  takePendingCookies,
} from './server/identity/identified-cookie';
export {
  dayKey,
  utcDay,
  VISITOR_ID_LENGTH,
  visitorHash,
  type VisitorHashInput,
} from './server/identity/visitor-hash';
export {
  type CounterFold,
  type Dimension,
  type EventRow,
  type FoldPlan,
  NONE_KEY,
  OVERFLOW_KEY,
  planFold,
  type RawUpsertResult,
  referrerHostOf,
  type SessionFold,
  type TouchedSession,
  type VisitorFold,
} from './server/sink/fold';
export {
  CREATE_ANALYTICS_DAILY,
  CREATE_ANALYTICS_EVENT,
  createMigrationSource,
  DEFAULT_SCHEMA,
  isAnalyticsSchema,
  MAX_RETENTION_DAYS,
  MIGRATION_NAMESPACE,
  MIN_RETENTION_DAYS,
  scheduleRetentionDefinition,
} from './server/sink/migrations';
export { createNoopSink } from './server/sink/noop';
export {
  awaitFlush,
  createWriteQueue,
  FLUSH_BUDGET_MS,
  type WriteQueue,
  type WriteQueueDeps,
} from './server/sink/queue';
export { createPathCap, PATH_CAP_TTL_MS, PATH_COUNT_SQL, type PathCap } from './server/sink/path-cap';
export { createRetention, type Retention, SWEEP_INTERVAL_MS } from './server/sink/retention';
export { createDedupCache, DEDUP_CAPACITY, type DedupCache } from './server/sink/dedup-cache';
export {
  COUNTERS_SQL,
  createSink,
  RAW_UPSERT_SQL,
  SESSION_ENGAGEMENT_SQL,
  SESSIONS_SQL,
  SET_SCHEMA_SQL,
  type Sink,
  type SinkCounts,
  type SinkDeps,
  SINK_FAILURE_LOG_INTERVAL_MS,
  SWEEP_SQL,
  VISITORS_SQL,
} from './server/sink/sink';
export {
  type SanitizedAction,
  type SanitizedEvent,
  type SanitizedPageView,
  type SanitizeDrop,
  sanitizeBatch,
  type SanitizeResult,
} from './server/sanitize/sanitizer';
export * from './server/sanitize/vocabulary';
export {
  type AnalyticsBootstrap,
  buildBootstrap,
} from './server/http/bootstrap';
export {
  canonicalTs,
  ingest,
  INGEST_PATH,
  MAX_CLOCK_SKEW_MS,
  RATE_LIMIT_WINDOW_MS,
  registerIngestRoute,
  toRow,
} from './server/http/ingest.route';
export { loadKnownRoutes, toFileRoute } from './server/http/known-routes';
export {
  formSubmitRow,
  isRenderedPage,
  pageViewMiddleware,
  serverPageViewRow,
} from './server/http/page-view.middleware';
export {
  baseRow,
  countryOf,
  primaryLanguage,
  type RowContext,
  ROUTE_NONE,
  ROUTE_UNKNOWN,
  ROUTE_UNMATCHED,
  userIdOf,
} from './server/http/rows';
export { registerTrackerAsset } from './server/http/tracker-asset';
export {
  ANALYTICS_VISITOR_SLOT,
  attachPendingCookies,
  resolveVisitor,
  type VisitorIdentity,
} from './server/http/visitor';
export {
  type AnalyticsRuntime,
  analyticsRuntime,
  createRuntime,
  FALLBACK_APP_NAME,
  FLUSH_DEADLINE_MS,
  flushAnalytics,
  NOT_INSTALLED_MESSAGE,
  resolveAppName,
  type RuntimeInput,
  setAnalyticsRuntime,
} from './server/runtime';
export { track } from './server/track';
export { uuidv7 } from './server/uuidv7';
