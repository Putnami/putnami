/**
 * Export cadence of the telemetry plugin, shared with the Go runtime: metrics
 * leave every 60 seconds and spans in batches at most 30 seconds apart, or at
 * once when 512 spans wait. The plugin settings override the defaults, then
 * the standard OpenTelemetry variables. The defaults equal
 * protocols/telemetry/fixtures/conventions/export-contract.json.
 */

/** Overrides the metric export interval, in milliseconds. */
export const ENV_METRIC_EXPORT_INTERVAL = 'OTEL_METRIC_EXPORT_INTERVAL';
/** Overrides the span batch delay, in milliseconds. */
export const ENV_SPAN_BATCH_DELAY = 'OTEL_BSP_SCHEDULE_DELAY';
/** Overrides the number of spans that make a full batch. */
export const ENV_SPAN_BATCH_SIZE = 'OTEL_BSP_MAX_EXPORT_BATCH_SIZE';

/** Default metric export interval. */
export const DEFAULT_METRIC_INTERVAL_MS = 60_000;
/** Default longest wait of a completed span before its batch leaves. */
export const DEFAULT_SPAN_BATCH_DELAY_MS = 30_000;
/** Default number of spans that make a full batch, pushed without waiting for the delay. */
export const MAX_SPAN_BATCH_SIZE = 512;

/** The longest period `setInterval` honors; a longer one fires at once. */
const MAX_TIMER_MS = 2_147_483_647;

type Environment = Readonly<Record<string, string | undefined>>;

/** The variable `name` as a positive integer written in digits only, as the Go runtime reads it. */
function envPositiveInt(name: string, env: Environment): number | undefined {
  const raw = env[name]?.trim() ?? '';
  if (!/^\d+$/.test(raw)) return undefined;
  const value = Number(raw);
  return Number.isSafeInteger(value) && value > 0 ? value : undefined;
}

/**
 * An export period in milliseconds: `configuredS` seconds when positive, then
 * the variable `name` in milliseconds, then `defaultMs`, capped at the longest
 * period a timer holds.
 */
export function cadenceMs(
  configuredS: number | undefined,
  name: string,
  defaultMs: number,
  env: Environment = process.env,
): number {
  const ms =
    configuredS !== undefined && configuredS > 0 ? configuredS * 1000 : (envPositiveInt(name, env) ?? defaultMs);
  return Math.min(ms, MAX_TIMER_MS);
}

/** The number of spans that make a full batch: the variable when set, else 512. */
export function spanBatchSize(env: Environment = process.env): number {
  return envPositiveInt(ENV_SPAN_BATCH_SIZE, env) ?? MAX_SPAN_BATCH_SIZE;
}
