export {
  logsRequestFromEntries,
  marshalCanonical,
  metricsRequestFromBuckets,
  type RenderBucket,
  type TraceRenderRecord,
  tracesRequestFromSpans,
} from './otlp';
export { OtlpLogSink, type OtlpLogSinkOptions } from './otlp-log.sink';
export {
  type AttributedCounterAggregate,
  type AttributedHistogramAggregate,
  type HistogramAggregate,
  type MetricsBucket,
  type TelemetryAttributes,
  TelemetryCollector,
  type TelemetrySpanRecord,
} from './telemetry.collector';
export {
  type ActiveTelemetrySpan,
  attachTelemetryContext,
  startTelemetrySpan,
  type TelemetrySpanOptions,
  type TelemetrySpanResult,
  type TelemetryTraceContext,
} from './client-telemetry';
export { TelemetryConfig, type TelemetryOptions } from './telemetry.config';
export { TelemetryMiddleware } from './telemetry.middleware';
export { TelemetryPlugin, telemetry } from './telemetry.plugin';
export {
  buildMetricsBody,
  buildTracesBody,
  joinUrl,
  type MetricsSendOptions,
  type OtlpSendResult,
  postOtlp,
  sendMetrics,
  sendTraces,
} from './telemetry.sender';
export {
  getCollector,
  incCounter,
  incCounterWithAttributes,
  observeHistogram,
  observeHistogramWithAttributes,
  setCollector,
  setGauge,
} from './telemetry.utils';
