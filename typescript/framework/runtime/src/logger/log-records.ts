import type { LogLevel } from './logger.type';

/** Whether a logger emitted a record or dropped it below the minimum level. */
export type LogRecordOutcome = 'emitted' | 'dropped';

/** Receives each record a logger gets, with its level and outcome. */
export type LogRecordListener = (level: LogLevel, outcome: LogRecordOutcome) => void;

const listeners = new Set<LogRecordListener>();

/**
 * Calls `listener` once for every record any logger of the process receives,
 * with its level and whether it was emitted or dropped. The telemetry plugin
 * counts these as `log.records{level, outcome}`. Returns a function that
 * removes the listener.
 */
export function observeLogRecords(listener: LogRecordListener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Notifies the listeners of one record. A listener that throws is ignored. */
export function countLogRecord(level: LogLevel, emitted: boolean): void {
  if (listeners.size === 0) {
    return;
  }
  const outcome: LogRecordOutcome = emitted ? 'emitted' : 'dropped';
  for (const listener of listeners) {
    try {
      listener(level, outcome);
    } catch {
      // A counting failure never breaks logging.
    }
  }
}
