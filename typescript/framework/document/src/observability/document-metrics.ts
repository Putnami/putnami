import { useLogger } from '@putnami/runtime';
import { incCounter, observeHistogram, setGauge } from '@putnami/application';

export type DocumentOperation = 'get' | 'find' | 'exists' | 'save' | 'saveMany' | 'delete' | 'deleteMany';

export interface DocumentMetrics {
  operation: DocumentOperation;
  collection: string;
  duration: number;
  rowCount?: number;
}

let activeTransactionCount = 0;

function setActiveTransactionCount(count: number): void {
  activeTransactionCount = Math.max(0, count);
  setGauge('document.transaction.active', activeTransactionCount);
}

export function recordDocumentOp(metrics: DocumentMetrics): void {
  const { operation, collection, duration, rowCount } = metrics;

  incCounter(`document.${operation}.${collection}`);
  observeHistogram(`document.${operation}.${collection}.duration`, duration);
  observeHistogram('document.operation.duration', duration);

  useLogger('document').debug(`${operation} ${collection}`, {
    operation,
    collection,
    duration,
    ...(rowCount !== undefined && { rowCount }),
  });
}

export function recordDocumentError(
  operation: DocumentOperation,
  collection: string,
  duration: number,
  error: unknown,
): void {
  incCounter(`document.${operation}.${collection}.error`);
  incCounter('document.operation.error');
  observeHistogram(`document.${operation}.${collection}.duration`, duration);
  observeHistogram('document.operation.duration', duration);

  useLogger('document').error(`${operation} ${collection} failed`, {
    operation,
    collection,
    duration,
    error: error instanceof Error ? error.message : String(error),
  });
}

export function recordSlowDocumentOp(metrics: DocumentMetrics, thresholdMs: number): void {
  if (metrics.duration >= thresholdMs) {
    incCounter('document.operation.slow');
    useLogger('document').warn(
      `Slow document operation: ${metrics.operation} ${metrics.collection} (${metrics.duration}ms >= ${thresholdMs}ms)`,
      { ...metrics, thresholdMs },
    );
  }
}

export function recordBackendCreated(name: string): void {
  incCounter('document.backend.created');
  useLogger('document').debug('Document backend created', { store: name });
}

export function recordBackendClosed(name: string): void {
  incCounter('document.backend.closed');
  useLogger('document').debug('Document backend closed', { store: name });
}

export function recordBackendCount(count: number): void {
  setGauge('document.backend.count', count);
}

export function recordTransactionStarted(store: string): void {
  incCounter('document.transaction.started');
  setActiveTransactionCount(activeTransactionCount + 1);
  useLogger('document').debug('Document transaction started', { store });
}

export function recordTransactionCommitted(store: string, duration: number): void {
  incCounter('document.transaction.committed');
  observeHistogram('document.transaction.duration', duration);
  setActiveTransactionCount(activeTransactionCount - 1);
  useLogger('document').debug('Document transaction committed', { store, duration });
}

export function recordTransactionRolledBack(store: string, duration: number, error?: unknown): void {
  incCounter('document.transaction.rolled_back');
  observeHistogram('document.transaction.duration', duration);
  setActiveTransactionCount(activeTransactionCount - 1);
  useLogger('document').debug('Document transaction rolled back', {
    store,
    duration,
    ...(error !== undefined && { error: error instanceof Error ? error.message : String(error) }),
  });
}
