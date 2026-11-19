import { type LogLevel, type LogRecordOutcome, observeLogRecords } from '@putnami/runtime';
import type { TelemetryCollector } from './telemetry.collector';

/**
 * The log-records convention of `protocols/telemetry/fixtures/conventions/log-records.json`,
 * shared with the Go runtime.
 */
export const LOG_RECORDS_METRIC = 'log.records';
const LOG_RECORDS_DESCRIPTION = 'Log records by level and outcome';

/**
 * Counts every record the process's loggers receive in `collector` as the
 * counter `log.records{level, outcome}`. Returns the function that stops
 * counting.
 */
export function countLogRecords(collector: TelemetryCollector): () => void {
  collector.describeMetric(LOG_RECORDS_METRIC, { description: LOG_RECORDS_DESCRIPTION });
  return observeLogRecords((level: LogLevel, outcome: LogRecordOutcome) => {
    collector.incCounterWithAttributes(LOG_RECORDS_METRIC, 1, { level, outcome });
  });
}
