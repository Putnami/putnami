# Logging

Complete guide to context-aware logging in `@putnami/runtime`.

## Overview

The logging system provides:

- Context-aware logging with automatic `traceId` and `logContext` propagation
- Structured JSON output for Google Cloud Logging / log aggregation
- Buffered logging with context correlation (group logs by request)
- Log level filtering
- Named loggers for organization
- Global exception handling for uncaught errors
- Pluggable sink architecture for custom outputs

## Basic Usage

### Getting a Logger

Use `useLogger()` to get a logger instance:

```typescript
import { useLogger } from '@putnami/runtime';

const logger = useLogger('my-service');
logger.info('Processing request');
logger.error('Failed to process', error);
```

### Log Levels

The logger supports standard log levels:

```typescript
const logger = useLogger('my-service');

logger.debug('Debug information');     // Development only
logger.info('Informational message');  // General information
logger.warn('Warning message');        // Warning
logger.error('Error occurred', error); // Errors
```

### Error Logging

Error objects are automatically extracted and structured:

```typescript
try {
  await riskyOperation();
} catch (error) {
  logger.error('Operation failed', error);
  // LogEntry includes: { error: { name, message, stack } }
}

// Error as message also works
logger.error(new Error('boom'));
// LogEntry: { message: 'Error: boom', error: { name: 'Error', ... } }
```

### Extra Data

Additional parameters are captured as structured data:

```typescript
logger.info('Request handled', { status: 200, duration: 42 });
// LogEntry includes: { data: [{ status: 200, duration: 42 }] }
```

## Context-Aware Logging

Loggers automatically include `traceId` and `logContext` from the current async context:

```typescript
import { useLogger, runInContext } from '@putnami/runtime';

await runInContext({ traceId: 'req-123' }, async () => {
  const logger = useLogger('user-service');

  logger.info('Processing user request');
  // Output includes traceId: 'req-123' automatically
});
```

## Adding Context Data

Use the `with()` method to add contextual data to logs.

Inside an async context, `with()` writes to the request-scoped log context and
all loggers in that context see the field:

```typescript
const logger = useLogger('user-service');

await runInContext({}, async () => {
  logger.with('userId', '123');
  logger.with('requestId', 'abc');
  logger.info('Processing request');
});
// All logs now include userId and requestId in the context field
```

Outside an async context, `with()` is immutable and returns a new logger. Chain
or reassign the returned logger to accumulate fields:

```typescript
const requestLogger = logger.with('userId', '123').with('requestId', 'abc');
requestLogger.info('Processing request');
// The original logger is unchanged
```

### Merging Context Data

By default, `with()` merges object values:

```typescript
const requestLogger = logger
  .with('metadata', { ip: '127.0.0.1' })
  .with('metadata', { userAgent: 'Mozilla/5.0' });
// metadata is now { ip: '127.0.0.1', userAgent: 'Mozilla/5.0' }
```

### Replacing Context Data

Use `replace: true` to replace existing values:

```typescript
const requestLogger = logger
  .with('metadata', { ip: '127.0.0.1' })
  .with('metadata', { userAgent: 'Mozilla/5.0' }, { replace: true });
// metadata is now { userAgent: 'Mozilla/5.0' } (ip is gone)
```

## Secret Redaction

The logger redacts sensitive values before they reach any sink, so credentials
passed via object messages, log params, or `with()` never land in stdout / Cloud
Logging in plaintext. Any object property whose key matches a case-insensitive
denylist has its value replaced with `***`, including nested objects and arrays:

```typescript
logger.info('connecting', { host: 'localhost', password: 'hunter2' });
// Output: { ..., "host": "localhost", "password": "***" }

const requestLogger = logger.with('auth', { token: 'abc', scheme: 'bearer' });
requestLogger.info('request');
// context: { auth: { token: '***', scheme: 'bearer' } }
```

The default denylist is `password`, `token`, `apiKey`, `authorization`, and
`secret`. Redaction never mutates the caller's object or the live request
`logContext` — it operates on a fresh copy.

Extend or replace the denylist when you have other sensitive field names:

```typescript
import { addSensitiveKeys, setSensitiveKeys } from '@putnami/runtime';

addSensitiveKeys('ssn', 'creditCard');  // keep defaults, add more
setSensitiveKeys(['password', 'apiKey']); // replace the list entirely
```

Redaction matches by key name only — it does not inspect schema `Sensitive()`
markers at log time, since logged values are plain objects with no attached
schema. Mark config fields `Sensitive()` for config-display redaction, and rely
on the key denylist for the log path.

## Named Loggers

Name your loggers for filtering and organization:

```typescript
const userLogger = useLogger('user-service');
const dbLogger = useLogger('database');
const apiLogger = useLogger('api-client');

userLogger.info('User created');
dbLogger.info('Query executed');
apiLogger.info('API call made');
```

The `named()` method creates a child logger that shares the same sinks:

```typescript
const base = useLogger('app');
const child = base.named('auth');
child.info('Token validated'); // logger: 'auth'
```

## Configuration

### Log Level

The default log level is `info`. Messages below the configured level are dropped.

Set the log level via the `LOG_LEVEL` environment variable:

```bash
# Show all logs including debug
LOG_LEVEL=debug bun run src/main.ts

# Default — info and above
LOG_LEVEL=info bun run src/main.ts

# Warnings and errors only
LOG_LEVEL=warn bun run src/main.ts
```

When using `putnami serve`, the `--debug` flag automatically sets `LOG_LEVEL=debug` in the served application:

```bash
# Application receives LOG_LEVEL=debug
bunx putnami serve my-app --debug
```

You can also configure the level in YAML config files (`conf/.env.local.yaml`):

```yaml
logger:
  level: debug
```

### Other Options

Configure additional logger settings in YAML config files:

```yaml
logger:
  json: true          # JSON output for production / Cloud Logging (default: true)
  buffer: false       # Enable buffered logging (default: false)
  bufferMaxSize: 100  # Entries per context before flush
  bufferFlushInterval: 5000  # ms between flushes
```

## Output Formats

### Console (default)

Human-readable text output:

```
[req-123] [INFO] [http] Request handled {"status":200,"duration":42}
```

- `traceId` in brackets prefix
- Level and logger name in brackets
- Message followed by data

### JSON

Structured JSON for log aggregation (Google Cloud Logging compatible):

```json
{
  "severity": "INFO",
  "message": "Request handled",
  "timestamp": "2024-01-01T12:00:00.000Z",
  "logger": "http",
  "traceId": "req-123",
  "status": 200,
  "duration": 42
}
```

Enable with `LOGGER_JSON=true`.

## Buffered Logging

When enabled, logs are grouped by `traceId` and flushed together. This enables correlating all logs from a single request/event.

```bash
LOGGER_BUFFER=true bun run src/main.ts
```

**Flush triggers:**

- **Size**: buffer for a context exceeds `LOGGER_BUFFER_MAX_SIZE` (default: 100)
- **Timer**: every `LOGGER_BUFFER_FLUSH_INTERVAL` ms (default: 5000)
- **Error**: an error-level entry immediately flushes the entire context buffer
- **Shutdown**: `close()` flushes all remaining buffers

The error flush behavior means that when a 500 occurs, all preceding debug/info/warn logs for that request are flushed alongside the error — giving full context for debugging.

Entries without a `traceId` pass through immediately (no grouping possible).

## Global Exception Handler

The application automatically installs global handlers for `uncaughtException` and `unhandledRejection` during `start()`. These log through the application logger, which means:

- Errors inside a `runInContext` scope include `traceId` and `logContext`
- Errors are structured consistently with all other logs
- Handlers are cleaned up during `stop()`

No configuration needed — this is automatic when using `Application`.

## Sinks

The logger writes to one or more `LogSink` implementations:

| Sink | Description |
|------|-------------|
| `ConsoleSink` | Text output to stdout/stderr |
| `JsonSink` | Structured JSON to stdout (Cloud Logging compatible) |
| `BufferSink` | Groups entries by traceId, flushes by size/time/error |
| `MemorySink` | In-memory capture for tests |

### Custom Sinks

Implement the `LogSink` interface:

```typescript
import { type LogSink, type LogEntry } from '@putnami/runtime';

class MyCustomSink implements LogSink {
  write(entry: LogEntry): void {
    // Send to your logging service
  }

  async flush(): Promise<void> {
    // Flush buffered entries
  }

  async close(): Promise<void> {
    // Clean up resources
  }
}
```

### Using Custom Sinks

```typescript
import { Logger } from '@putnami/runtime';

const logger = new Logger([new MyCustomSink(), new JsonSink()]);
logger.info('Sent to both sinks');
```

## Testing

Use `MemoryLogger` to capture and assert on log output in tests:

```typescript
import { MemoryLogger } from '@putnami/runtime/testing';

const logger = new MemoryLogger('test');
logger.info('hello');
logger.error('failed', new Error('boom'));

expect(logger.entries).toHaveLength(2);
expect(logger.entries[0].message).toBe('hello');
expect(logger.entries[1].error?.name).toBe('Error');

logger.clear(); // Reset
```

## LogEntry Structure

Every log method produces a `LogEntry`:

```typescript
interface LogEntry {
  level: LogLevel;                      // 'debug' | 'info' | 'warn' | 'error'
  message: string;                      // Formatted message
  timestamp: string;                    // ISO 8601
  logger?: string;                      // Logger name
  traceId?: string;                     // From async context
  context?: Record<string, unknown>;    // From logContext + with()
  error?: {                             // Extracted from Error objects
    name: string;
    message: string;
    stack?: string;
  };
  data?: unknown[];                     // Extra parameters
}
```

## Best Practices

1. **Name your loggers**: Use descriptive names like `'user-service'` not `'logger'`
2. **Add context early**: Set context data at the start of request handling
3. **Use appropriate levels**: `debug` for development, `info` for normal flow, `warn` for issues, `error` for failures
4. **Include relevant data**: Add context that helps debug issues
5. **Don't rely on redaction alone**: Common secret keys (`password`, `token`, …) are auto-redacted to `***`, but treat this as a safety net — still avoid logging PII or secrets under non-standard keys, and extend the denylist via `addSensitiveKeys()` when needed
6. **Use JSON in production**: Enable `LOGGER_JSON=true` for log aggregation
7. **Enable buffering for debugging**: `LOGGER_BUFFER=true` to correlate request logs

## Next Steps

- Learn about [Context Management](context-management.md) for request-scoped logging
- Explore [Error Handling](error-handling.md) for error logging patterns
- Check the [API Reference](api-reference.md) for complete logger API
