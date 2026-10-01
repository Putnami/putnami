/**
 * OTLP Log Sink — pushes log records to a collector as OTLP/JSON.
 *
 * Opt-in: attach it when constructing the application logger, exactly like the
 * console and JSON sinks. Entries are batched and flushed on a background ticker
 * (the protocol's 10s default) and on close, so the logging path never blocks on
 * the network. A bounded queue drops the oldest records if the collector wedges.
 *
 *   const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 'checkout' });
 *   const logger = new Logger([new JsonSink(), sink], 'checkout');
 *   // on shutdown: await sink.close();
 */

import type { LogEntry, LogSink } from '@putnami/runtime';
import { logsRequestFromEntries, marshalCanonical, PATH_LOGS } from './otlp';
import { postOtlp } from './telemetry.sender';

const DEFAULT_FLUSH_INTERVAL_MS = 10_000;
const DEFAULT_MAX_QUEUE = 10_000;

export interface OtlpLogSinkOptions {
  /** Collector base URL (e.g. "https://collector:4318"). */
  endpoint: string;
  /** Service name reported as the OTLP resource's service.name. */
  serviceName: string;
  /** Optional service.version resource attribute. */
  serviceVersion?: string;
  /** Bearer token for the collector. */
  bearer?: string;
  /** Extra headers (e.g. tenant routing). */
  headers?: Record<string, string>;
  /** Periodic flush cadence in ms. Defaults to 10s. */
  flushIntervalMs?: number;
  /** Maximum buffered records. Defaults to 10000; oldest are dropped beyond it. */
  maxQueue?: number;
}

export class OtlpLogSink implements LogSink {
  private buf: LogEntry[] = [];
  private timer: ReturnType<typeof setInterval> | undefined;
  private readonly maxQueue: number;

  constructor(private readonly options: OtlpLogSinkOptions) {
    this.maxQueue = options.maxQueue ?? DEFAULT_MAX_QUEUE;
    const interval = options.flushIntervalMs ?? DEFAULT_FLUSH_INTERVAL_MS;
    this.timer = setInterval(() => {
      // Fire-and-forget; flush already swallows collector errors.
      this.flush().catch(() => undefined);
    }, interval);
    // Do not keep the process alive for telemetry.
    if (this.timer && typeof this.timer === 'object' && 'unref' in this.timer) {
      this.timer.unref();
    }
  }

  /** Buffer one log entry. Never touches the network. */
  write(entry: LogEntry): void {
    this.buf.push(entry);
    if (this.buf.length > this.maxQueue) {
      this.buf = this.buf.slice(this.buf.length - this.maxQueue);
    }
  }

  /** Drain the buffer and POST it as OTLP/JSON. Best-effort. */
  async flush(): Promise<void> {
    if (this.buf.length === 0) return;
    const entries = this.buf;
    this.buf = [];
    const body = marshalCanonical(
      logsRequestFromEntries(entries, {
        serviceName: this.options.serviceName,
        serviceVersion: this.options.serviceVersion,
      }),
    );
    await postOtlp(this.options.endpoint, PATH_LOGS, body, {
      bearer: this.options.bearer,
      headers: this.options.headers,
    });
  }

  /** Stop the background flusher and perform a final flush. */
  async close(): Promise<void> {
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
    await this.flush();
  }
}
