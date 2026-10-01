/**
 * Telemetry Utils — Public Metrics API
 *
 * Functional helpers that delegate to the global TelemetryCollector.
 * All functions are no-ops when telemetry is not initialized, so they
 * are safe to call unconditionally.
 */

import { useLogger } from '@putnami/runtime';
import type { TelemetryAttributes, TelemetryCollector } from './telemetry.collector';

// ---------------------------------------------------------------------------
// Global collector reference (set by TelemetryPlugin)
// ---------------------------------------------------------------------------

// The metrics helpers below (incCounter/setGauge/observeHistogram) are a global
// functional API callable from anywhere, so they resolve a process-wide
// collector rather than a DI-scoped one. This means a single process is expected
// to run at most one telemetry-enabled Application; running two clobbers this
// reference. setCollector() warns on that misuse, and clearCollector() only
// resets the global when it still points at the caller's collector, so one
// app's shutdown cannot null out another app's collector.
let _collector: TelemetryCollector | undefined;

/** @internal Used by TelemetryPlugin to wire the active collector. */
export function setCollector(collector: TelemetryCollector | undefined): void {
  if (collector && _collector && _collector !== collector) {
    useLogger('telemetry').warn(
      'A telemetry collector is already active in this process; overwriting it. ' +
        'The metrics API (incCounter/setGauge/observeHistogram) is process-global, ' +
        'so running more than one telemetry-enabled Application per process is unsupported.',
    );
  }
  _collector = collector;
}

/**
 * @internal Reset the global collector, but only if it still points at the
 * provided instance. Prevents one Application's shutdown from clearing a
 * collector that another Application installed afterwards.
 */
export function clearCollector(collector: TelemetryCollector): void {
  if (_collector === collector) {
    _collector = undefined;
  }
}

/** @internal Get the current collector (for testing). */
export function getCollector(): TelemetryCollector | undefined {
  return _collector;
}

// ---------------------------------------------------------------------------
// Counter
// ---------------------------------------------------------------------------

/**
 * Increment a counter metric.
 *
 * @param name  - Dot-delimited metric name (e.g. `"app.orders.created"`).
 * @param value - Increment amount. Defaults to 1.
 *
 * @example
 * ```typescript
 * incCounter('app.orders.created');
 * incCounter('app.emails.sent', 3);
 * ```
 */
export function incCounter(name: string, value = 1): void {
  _collector?.incCounter(name, value);
}

/** @internal Record a counter with framework-owned bounded attributes. */
export function incCounterWithAttributes(name: string, value: number, attributes: TelemetryAttributes): void {
  _collector?.incCounterWithAttributes(name, value, attributes);
}

// ---------------------------------------------------------------------------
// Gauge
// ---------------------------------------------------------------------------

/**
 * Set a gauge metric to an absolute value.
 *
 * @param name  - Dot-delimited metric name.
 * @param value - Current value.
 *
 * @example
 * ```typescript
 * setGauge('app.queue.size', pendingJobs.length);
 * ```
 */
export function setGauge(name: string, value: number): void {
  _collector?.setGauge(name, value);
}

// ---------------------------------------------------------------------------
// Histogram
// ---------------------------------------------------------------------------

/**
 * Record an observation in a histogram metric.
 *
 * @param name  - Dot-delimited metric name.
 * @param value - Observed value (e.g. duration in ms).
 *
 * @example
 * ```typescript
 * const start = Date.now();
 * await processOrder(order);
 * observeHistogram('app.order.processing.duration', Date.now() - start);
 * ```
 */
export function observeHistogram(name: string, value: number): void {
  _collector?.observeHistogram(name, value);
}

/** @internal Record a histogram with framework-owned bounded attributes. */
export function observeHistogramWithAttributes(name: string, value: number, attributes: TelemetryAttributes): void {
  _collector?.observeHistogramWithAttributes(name, value, attributes);
}
