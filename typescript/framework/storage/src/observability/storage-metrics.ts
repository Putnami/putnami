import { useLogger } from '@putnami/runtime';
import { incCounter, observeHistogram, setGauge } from '@putnami/application';
import type { StorageOperation } from '../client/storage.types';

/**
 * Structured metrics captured for each storage operation.
 */
export interface StorageMetrics {
  operation: StorageOperation;
  bucket: string;
  /** Duration in milliseconds */
  duration: number;
  /** Bytes transferred (uploaded or downloaded) */
  bytes?: number;
  /** Object key involved */
  key?: string;
}

// ---------------------------------------------------------------------------
// Operation metrics
// ---------------------------------------------------------------------------

/**
 * Record a successful storage operation with structured logging and telemetry.
 *
 * Emits:
 * - Counter: `storage.{operation}.{bucket}`
 * - Histogram: `storage.{operation}.{bucket}.duration` (ms)
 * - Histogram: `storage.operation.duration` (aggregate)
 * - Histogram: `storage.{operation}.bytes` (when applicable)
 */
export function recordStorageOp(metrics: StorageMetrics): void {
  const { operation, bucket, duration, bytes } = metrics;

  incCounter(`storage.${operation}.${bucket}`);
  observeHistogram(`storage.${operation}.${bucket}.duration`, duration);
  observeHistogram('storage.operation.duration', duration);

  if (bytes !== undefined) {
    observeHistogram(`storage.${operation}.bytes`, bytes);
  }

  useLogger('storage').debug(`${operation} ${bucket}`, {
    operation,
    bucket,
    duration,
    ...(bytes !== undefined && { bytes }),
    ...(metrics.key && { key: metrics.key }),
  });
}

/**
 * Record a failed storage operation.
 *
 * Emits:
 * - Counter: `storage.{operation}.{bucket}.error`
 * - Counter: `storage.operation.error` (aggregate)
 * - Histogram: `storage.operation.duration` (even failures contribute to latency)
 */
export function recordStorageError(
  operation: StorageOperation,
  bucket: string,
  duration: number,
  error: unknown,
): void {
  incCounter(`storage.${operation}.${bucket}.error`);
  incCounter('storage.operation.error');
  observeHistogram('storage.operation.duration', duration);

  useLogger('storage').error(`${operation} ${bucket} failed`, {
    operation,
    bucket,
    duration,
    error: error instanceof Error ? error.message : String(error),
  });
}

/**
 * Emit a warning when a storage operation exceeds the configured slow threshold.
 */
export function recordSlowStorageOp(metrics: StorageMetrics, thresholdMs: number): void {
  if (metrics.duration >= thresholdMs) {
    incCounter('storage.operation.slow');
    useLogger('storage').warn(
      `Slow storage operation: ${metrics.operation} ${metrics.bucket} (${metrics.duration}ms >= ${thresholdMs}ms)`,
      { ...metrics, thresholdMs },
    );
  }
}

// ---------------------------------------------------------------------------
// Client lifecycle metrics
// ---------------------------------------------------------------------------

/**
 * Record that a storage client was created.
 */
export function recordClientCreated(bucket: string): void {
  incCounter('storage.client.created');
  useLogger('storage').debug('Storage client created', { bucket });
}

/**
 * Record that a storage client was closed.
 */
export function recordClientClosed(bucket: string): void {
  incCounter('storage.client.closed');
  useLogger('storage').debug('Storage client closed', { bucket });
}

/**
 * Set the current number of active storage clients.
 */
export function recordClientCount(count: number): void {
  setGauge('storage.client.count', count);
}
