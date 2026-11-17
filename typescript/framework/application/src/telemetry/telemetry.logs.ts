/**
 * Logs from the environment — the OTLP log sink `application()` attaches to
 * the root logger when the environment names a collector (ADR 0007).
 */

import { type Logger, useLogger } from '@putnami/runtime';
import { OtlpLogSink } from './otlp-log.sink';
import { environmentEndpoint } from './telemetry.environment';

/** One application's hold on the log sink the environment attached. */
export interface LogSinkHold {
  /** Push the records the sink buffered. */
  flush(): Promise<void>;
  /**
   * Give the hold up. The last holder detaches the sink from its logger and
   * closes it, flushing its buffer; an earlier one flushes it. Idempotent.
   */
  release(): Promise<void>;
}

interface Attachment {
  sink: OtlpLogSink;
  holders: number;
  detach: () => void;
}

const attachments = new WeakMap<Logger, Attachment>();

/**
 * Hold the OTLP log sink the environment attaches to `logger`, the root
 * logger by default: the first holder attaches it for the collector
 * `OTEL_EXPORTER_OTLP_ENDPOINT` names, and every application started in the
 * same process shares it. Answers `undefined`, attaching nothing, when the
 * environment names no collector, turns telemetry off, or the logger holds an
 * OTLP log sink the application configured itself.
 */
export function attachLogSinkFromEnvironment(logger: Logger = useLogger()): LogSinkHold | undefined {
  const endpoint = environmentEndpoint();
  if (!endpoint) return undefined;

  let attachment = attachments.get(logger);
  if (!attachment || !logger.sinks.includes(attachment.sink)) {
    if (logger.sinks.some((sink) => sink instanceof OtlpLogSink)) return undefined;
    const sink = new OtlpLogSink({ endpoint });
    attachment = { sink, holders: 0, detach: logger.addSink(sink) };
    attachments.set(logger, attachment);
  }
  const held = attachment;
  held.holders += 1;

  let released = false;
  return {
    flush: () => held.sink.flush(),
    async release() {
      if (released) return;
      released = true;
      held.holders -= 1;
      if (held.holders > 0) {
        await held.sink.flush();
        return;
      }
      if (attachments.get(logger) === held) attachments.delete(logger);
      held.detach();
      await held.sink.close();
    },
  };
}
