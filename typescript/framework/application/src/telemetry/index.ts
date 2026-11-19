export {
  type ActiveTelemetrySpan,
  attachTelemetryContext,
  startTelemetrySpan,
  type TelemetrySpanOptions,
  type TelemetrySpanResult,
  type TelemetryTraceContext,
} from './client-telemetry';
export {
  EXPORT_DIRECTIVE_HEADER,
  type ExportDirective,
  MAX_DIRECTIVE_TTL_SECONDS,
  MAX_RETRY_AFTER_SECONDS,
  parseExportDirective,
  retryAfterDelayMs,
} from './export-directive';
export {
  ATTR_HTTP_METHOD,
  ATTR_HTTP_ROUTE,
  ATTR_HTTP_STATUS_CODE,
  HTTP_SERVER_DURATION,
  HTTP_SERVER_REQUEST_COUNT,
} from './http-server-metrics';
export {
  logsRequestFromEntries,
  marshalCanonical,
  metricsRequestFromBuckets,
  type RenderBucket,
  type RenderHistogram,
  type TraceRenderRecord,
  tracesRequestFromSpans,
} from './otlp';
export { OtlpLogSink, type OtlpLogSinkOptions } from './otlp-log.sink';
export {
  type AttributedCounterAggregate,
  type AttributedHistogramAggregate,
  HISTOGRAM_BOUNDS,
  type HistogramAggregate,
  type MetricDescription,
  type MetricsBucket,
  type TelemetryAttributes,
  TelemetryCollector,
  type TelemetrySpanRecord,
} from './telemetry.collector';
export { TelemetryConfig, type TelemetryOptions } from './telemetry.config';
export {
  ENV_OTLP_ENDPOINT,
  ENV_RESOURCE_ATTRIBUTES,
  ENV_SDK_DISABLED,
  ENV_SERVICE_NAME,
  parseResourceAttributes,
} from './telemetry.environment';
export { TelemetryMiddleware } from './telemetry.middleware';
export {
  DEFAULT_SAMPLING_REFRESH_MS,
  TelemetryPlugin,
  telemetry,
  telemetryFromEnvironment,
} from './telemetry.plugin';
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
export {
  ATTR_TRACE_DECISION_OUTCOME,
  DEFAULT_TRACE_SAMPLING,
  getTraceSampler,
  TRACE_DECISIONS,
  TRACE_SAMPLING_CONFIG_PATH,
  TraceSampler,
  type TraceSampling,
  TraceSamplingConfig,
  traceRatioPicks,
} from './trace-sampling';
