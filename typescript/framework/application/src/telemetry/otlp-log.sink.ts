/**
 * OTLP Log Sink — pushes log records to a collector as OTLP/JSON.
 *
 * `application()` attaches one to the root logger when the environment names a
 * collector (`OTEL_EXPORTER_OTLP_ENDPOINT`). Attach one yourself, like the
 * console and JSON sinks, to push to another collector. Entries are batched and
 * flushed on a background ticker (the protocol's 10s default) and on close, so
 * the logging path never blocks on the network. A bounded queue drops the
 * oldest records if the collector wedges.
 *
 * The resource carries the service identity of the environment, as metrics
 * and traces do, and each push asks the registered telemetry token source for
 * its bearer unless a static `bearer` is set.
 *
 *   const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 'checkout' });
 *   const logger = new Logger([new JsonSink(), sink], 'checkout');
 *   // on shutdown: await sink.close();
 */

import type { LogEntry, LogSink } from '@putnami/runtime';
import { logsRequestFromEntries, marshalCanonical, PATH_LOGS } from './otlp';
import { CollectorAccess } from './telemetry.credential';
import { type ServiceIdentity, serviceIdentity } from './telemetry.environment';
import { postOtlp } from './telemetry.sender';

const DEFAULT_FLUSH_INTERVAL_MS = 10_000;
const DEFAULT_MAX_QUEUE = 10_000;

export interface OtlpLogSinkOptions {
  /** Collector base URL (e.g. "https://collector:4318"). */
  endpoint: string;
  /**
   * The OTLP resource's service.name when the environment names none:
   * `OTEL_SERVICE_NAME` and the `service.name` pair of
   * `OTEL_RESOURCE_ATTRIBUTES` win over it. Defaults to `unknown`.
   */
  serviceName?: string;
  /** Optional service.version resource attribute. */
  serviceVersion?: string;
  /** A static bearer for the collector; it wins over the registered telemetry token source. */
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
  private readonly identity: ServiceIdentity;
  private readonly access: CollectorAccess;

  constructor(private readonly options: OtlpLogSinkOptions) {
    this.maxQueue = options.maxQueue ?? DEFAULT_MAX_QUEUE;
    this.identity = serviceIdentity(options.serviceName);
    this.access = new CollectorAccess(options.endpoint, options.bearer);
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

  /**
   * Drain the buffer and POST it as OTLP/JSON. Best-effort: a batch without a
   * credential is dropped, and no collector answer rejects.
   */
  async flush(): Promise<void> {
    if (this.buf.length === 0) return;
    const entries = this.buf;
    this.buf = [];
    const credential = await this.access.credential();
    if (!credential.ok) return;
    const body = marshalCanonical(
      logsRequestFromEntries(entries, {
        serviceName: this.identity.serviceName,
        serviceVersion: this.options.serviceVersion,
        resourceAttributes: this.identity.resourceAttributes,
      }),
    );
    const result = await postOtlp(this.options.endpoint, PATH_LOGS, body, {
      bearer: credential.bearer,
      headers: this.options.headers,
    });
    this.access.observe(result);
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
