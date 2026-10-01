export { BufferSink, type BufferSinkOptions } from './buffer.sink';
export { ConsoleSink } from './console.sink';
export { installExceptionHandler } from './exception-handler';
// buildJsonRecord is the exact record the production JSON sink prints. It is
// exported because a serve extension's forwarder reads that record — not the
// LogEntry — so any test asserting what the forwarder sees must render through
// the real sink path rather than intercepting console.log.
export { buildJsonRecord, JsonSink } from './json.sink';
export { Logger, useLogger, resetDefaultLogger, setRootLogger, MAX_APPENDED_FIELD_VALUES } from './logger';
export { LoggerConfig, getLoggerConfig, resetLoggerConfig, shouldLog } from './logger.config';
export { REDACTED, redact, setSensitiveKeys, addSensitiveKeys } from './redact';
export { type LogEntry, type LogLevel, type LogSink, type WithOptions, LEVEL_PRIORITY } from './logger.type';
// MemoryLogger/MemorySink are test helpers — import from `@putnami/runtime/testing`.
// buildEntry is an internal helper consumed only by Logger itself.
