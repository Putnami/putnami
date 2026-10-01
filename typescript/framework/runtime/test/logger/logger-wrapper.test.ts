// biome-ignore-all lint/suspicious/noConsole: Testing JsonSink which writes to console.log

import { beforeEach, describe, expect, it } from 'bun:test';
import { restoreEnv } from '@putnami/utils';
import { BufferSink } from '../../src/logger/buffer.sink';
import { ConsoleSink } from '../../src/logger/console.sink';
import { JsonSink } from '../../src/logger/json.sink';
import { Logger } from '../../src/logger/logger';
import { MemorySink } from '../../src/logger/memory.logger';

describe('ConsoleSink', () => {
  it('should format log entries as text', () => {
    const output: string[] = [];
    const originalWrite = process.stdout.write;
    process.stdout.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      sink.write({
        level: 'info',
        message: 'hello world',
        timestamp: '2024-01-01T00:00:00.000Z',
        logger: 'http',
        traceId: 'trace-1',
      });

      expect(output[0]).toContain('[trace-1]');
      expect(output[0]).toContain('[INFO]');
      expect(output[0]).toContain('[http]');
      expect(output[0]).toContain('hello world');
    } finally {
      process.stdout.write = originalWrite;
    }
  });

  it('should write errors to stderr', () => {
    const output: string[] = [];
    const originalWrite = process.stderr.write;
    process.stderr.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      sink.write({
        level: 'error',
        message: 'failed',
        timestamp: '2024-01-01T00:00:00.000Z',
      });

      expect(output[0]).toContain('[ERROR]');
      expect(output[0]).toContain('failed');
    } finally {
      process.stderr.write = originalWrite;
    }
  });

  it('should include string data items', () => {
    const output: string[] = [];
    const originalWrite = process.stdout.write;
    process.stdout.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      sink.write({
        level: 'info',
        message: 'request',
        timestamp: '2024-01-01T00:00:00.000Z',
        data: ['extra-string', { key: 'value' }],
      });

      expect(output[0]).toContain('extra-string');
      expect(output[0]).toContain('"key":"value"');
    } finally {
      process.stdout.write = originalWrite;
    }
  });

  it('should handle non-serializable data gracefully', () => {
    const output: string[] = [];
    const originalWrite = process.stdout.write;
    process.stdout.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      const circular: Record<string, unknown> = {};
      circular.self = circular;
      sink.write({
        level: 'info',
        message: 'msg',
        timestamp: '2024-01-01T00:00:00.000Z',
        data: [circular],
      });

      expect(output[0]).toContain('msg');
      expect(output[0]).toContain('[object Object]');
    } finally {
      process.stdout.write = originalWrite;
    }
  });

  it('should include context as JSON', () => {
    const output: string[] = [];
    const originalWrite = process.stdout.write;
    process.stdout.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      sink.write({
        level: 'info',
        message: 'with ctx',
        timestamp: '2024-01-01T00:00:00.000Z',
        context: { userId: 'u-1', role: 'admin' },
      });

      expect(output[0]).toContain('with ctx');
      expect(output[0]).toContain('"userId":"u-1"');
    } finally {
      process.stdout.write = originalWrite;
    }
  });

  it('should skip empty context object', () => {
    const output: string[] = [];
    const originalWrite = process.stdout.write;
    process.stdout.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      sink.write({
        level: 'info',
        message: 'no ctx',
        timestamp: '2024-01-01T00:00:00.000Z',
        context: {},
      });

      // Should only contain the message, not '{}'
      expect(output[0].trim()).toBe('[INFO] no ctx');
    } finally {
      process.stdout.write = originalWrite;
    }
  });

  it('should write warn to stderr', () => {
    const output: string[] = [];
    const originalWrite = process.stderr.write;
    process.stderr.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      sink.write({
        level: 'warn',
        message: 'caution',
        timestamp: '2024-01-01T00:00:00.000Z',
      });

      expect(output[0]).toContain('[WARN]');
      expect(output[0]).toContain('caution');
    } finally {
      process.stderr.write = originalWrite;
    }
  });

  it('should print error stack on stderr', () => {
    const output: string[] = [];
    const originalWrite = process.stderr.write;
    process.stderr.write = (chunk: string | Uint8Array) => {
      output.push(chunk.toString());
      return true;
    };

    try {
      const sink = new ConsoleSink();
      sink.write({
        level: 'error',
        message: 'Error: boom',
        timestamp: '2024-01-01T00:00:00.000Z',
        error: { name: 'Error', message: 'boom', stack: 'Error: boom\n    at test.ts:1' },
      });

      expect(output.length).toBeGreaterThanOrEqual(2);
      expect(output[1]).toContain('Error: boom');
    } finally {
      process.stderr.write = originalWrite;
    }
  });
});

describe('JsonSink', () => {
  it('should output structured JSON', () => {
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };

    try {
      const sink = new JsonSink();
      sink.write({
        level: 'info',
        message: 'hello',
        timestamp: '2024-01-01T00:00:00.000Z',
        logger: 'test',
        traceId: 'trace-1',
        context: { userId: 'u-1' },
      });

      const parsed = JSON.parse(output[0]);
      expect(parsed.severity).toBe('INFO');
      expect(parsed.message).toBe('hello');
      expect(parsed.logger).toBe('test');
      expect(parsed.traceId).toBe('trace-1');
      expect(parsed.userId).toBe('u-1');
    } finally {
      console.log = originalLog;
    }
  });

  it('should include error details', () => {
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };

    try {
      const sink = new JsonSink();
      sink.write({
        level: 'error',
        message: 'Error: boom',
        timestamp: '2024-01-01T00:00:00.000Z',
        error: { name: 'Error', message: 'boom', stack: 'Error: boom\n    at test.ts:1' },
      });

      const parsed = JSON.parse(output[0]);
      expect(parsed.severity).toBe('ERROR');
      expect(parsed.error.name).toBe('Error');
      expect(parsed.error.stack).toContain('test.ts');
    } finally {
      console.log = originalLog;
    }
  });

  it('should format traceId as logging.googleapis.com/trace when GCP project is set', () => {
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };
    const originalProject = process.env['GOOGLE_CLOUD_PROJECT'];
    process.env['GOOGLE_CLOUD_PROJECT'] = 'my-project';

    try {
      const sink = new JsonSink();
      sink.write({
        level: 'info',
        message: 'hello',
        timestamp: '2024-01-01T00:00:00.000Z',
        traceId: 'abc123',
      });

      const parsed = JSON.parse(output[0]);
      expect(parsed['logging.googleapis.com/trace']).toBe('projects/my-project/traces/abc123');
      expect(parsed.traceId).toBeUndefined();
    } finally {
      console.log = originalLog;
      restoreEnv('GOOGLE_CLOUD_PROJECT', originalProject);
    }
  });

  it('should keep traceId as-is when no GCP project is set', () => {
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };
    const originalProject = process.env['GOOGLE_CLOUD_PROJECT'];
    const originalGcpProject = process.env['GCP_PROJECT'];
    delete process.env.GOOGLE_CLOUD_PROJECT;
    delete process.env.GCP_PROJECT;

    try {
      const sink = new JsonSink();
      sink.write({
        level: 'info',
        message: 'hello',
        timestamp: '2024-01-01T00:00:00.000Z',
        traceId: 'abc123',
      });

      const parsed = JSON.parse(output[0]);
      expect(parsed.traceId).toBe('abc123');
      expect(parsed['logging.googleapis.com/trace']).toBeUndefined();
    } finally {
      console.log = originalLog;
      restoreEnv('GOOGLE_CLOUD_PROJECT', originalProject);
      restoreEnv('GCP_PROJECT', originalGcpProject);
    }
  });

  it('should not reformat traceId that already contains a slash', () => {
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };
    const originalProject = process.env['GOOGLE_CLOUD_PROJECT'];
    process.env['GOOGLE_CLOUD_PROJECT'] = 'my-project';

    try {
      const sink = new JsonSink();
      sink.write({
        level: 'info',
        message: 'hello',
        timestamp: '2024-01-01T00:00:00.000Z',
        traceId: 'projects/other/traces/abc123',
      });

      const parsed = JSON.parse(output[0]);
      expect(parsed.traceId).toBe('projects/other/traces/abc123');
      expect(parsed['logging.googleapis.com/trace']).toBeUndefined();
    } finally {
      console.log = originalLog;
      restoreEnv('GOOGLE_CLOUD_PROJECT', originalProject);
    }
  });

  it('should merge single-object data as top-level fields', () => {
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };

    try {
      const sink = new JsonSink();
      sink.write({
        level: 'info',
        message: 'request',
        timestamp: '2024-01-01T00:00:00.000Z',
        data: [{ status: 200, duration: 42 }],
      });

      const parsed = JSON.parse(output[0]);
      expect(parsed.status).toBe(200);
      expect(parsed.duration).toBe(42);
    } finally {
      console.log = originalLog;
    }
  });

  it('should not throw on a circular non-plain object and still emit a parseable line', () => {
    // Regression: redact() leaves class instances untouched, so a
    // circular class instance reaches JSON.stringify in the JSON sink (the
    // production default). A plain stringify would throw and crash the caller.
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };

    try {
      // A non-plain object (class instance) holding a back-reference to itself,
      // passed as log data — exactly what redact() returns as-is.
      class Node {
        name = 'root';
        self?: Node;
      }
      const node = new Node();
      node.self = node;

      const logger = new Logger([new JsonSink()], 'graph');
      expect(() => logger.info('cycle', node)).not.toThrow();

      expect(output).toHaveLength(1);
      const parsed = JSON.parse(output[0]);
      expect(parsed.severity).toBe('INFO');
      expect(parsed.message).toBe('cycle');
      // The non-circular field survived; the back-reference was broken.
      expect(JSON.stringify(parsed)).toContain('root');
      expect(JSON.stringify(parsed)).toContain('[Circular]');
    } finally {
      console.log = originalLog;
    }
  });

  it('should break a circular reference without falsely flagging repeated siblings', () => {
    // The circular-safe replacer must only flag true ancestors on the path, not
    // a value that merely appears twice in sibling positions.
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };

    try {
      class Holder {
        shared = { id: 'shared-node' };
        first?: { ref: unknown };
        second?: { ref: unknown };
      }
      const holder = new Holder();
      // `shared` referenced twice (siblings, not a cycle) — must survive both.
      holder.first = { ref: holder.shared };
      holder.second = { ref: holder.shared };

      const logger = new Logger([new JsonSink()], 'graph');
      logger.info('siblings', holder);

      const parsed = JSON.parse(output[0]) as Record<string, unknown>;
      const first = parsed.first as { ref: { id: string } };
      const second = parsed.second as { ref: { id: string } };
      expect(first.ref.id).toBe('shared-node');
      expect(second.ref.id).toBe('shared-node');
      expect(output[0]).not.toContain('[Circular]');
    } finally {
      console.log = originalLog;
    }
  });
});

describe('BufferSink', () => {
  let delegate: MemorySink;

  beforeEach(() => {
    delegate = new MemorySink();
  });

  it('should pass through entries without traceId immediately', () => {
    const buffer = new BufferSink({ sink: delegate, flushInterval: 60_000 });
    buffer.write({ level: 'info', message: 'no trace', timestamp: '2024-01-01T00:00:00.000Z' });
    expect(delegate.entries).toHaveLength(1);
    buffer.close();
  });

  it('should buffer entries with traceId', () => {
    const buffer = new BufferSink({ sink: delegate, flushInterval: 60_000 });
    buffer.write({ level: 'info', message: 'msg', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-1' });
    expect(delegate.entries).toHaveLength(0);
    buffer.close();
  });

  it('should flush on error-level entry', () => {
    const buffer = new BufferSink({ sink: delegate, flushInterval: 60_000 });
    buffer.write({ level: 'info', message: 'before', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-1' });
    buffer.write({ level: 'debug', message: 'debug', timestamp: '2024-01-01T00:00:01.000Z', traceId: 't-1' });
    expect(delegate.entries).toHaveLength(0);

    buffer.write({ level: 'error', message: 'boom', timestamp: '2024-01-01T00:00:02.000Z', traceId: 't-1' });
    expect(delegate.entries).toHaveLength(3);
    expect(delegate.entries[0].message).toBe('before');
    expect(delegate.entries[1].message).toBe('debug');
    expect(delegate.entries[2].message).toBe('boom');
    buffer.close();
  });

  it('should flush when maxSize is reached', () => {
    const buffer = new BufferSink({ sink: delegate, maxSize: 2, flushInterval: 60_000 });
    buffer.write({ level: 'info', message: 'one', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-1' });
    expect(delegate.entries).toHaveLength(0);
    buffer.write({ level: 'info', message: 'two', timestamp: '2024-01-01T00:00:01.000Z', traceId: 't-1' });
    expect(delegate.entries).toHaveLength(2);
    buffer.close();
  });

  it('should flush all buffers on explicit flush()', async () => {
    const buffer = new BufferSink({ sink: delegate, flushInterval: 60_000 });
    buffer.write({ level: 'info', message: 'a', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-1' });
    buffer.write({ level: 'info', message: 'b', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-2' });
    expect(delegate.entries).toHaveLength(0);

    await buffer.flush();
    expect(delegate.entries).toHaveLength(2);
    buffer.close();
  });

  it('should isolate buffers by traceId', () => {
    const buffer = new BufferSink({ sink: delegate, flushInterval: 60_000 });
    buffer.write({ level: 'info', message: 'a1', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-1' });
    buffer.write({ level: 'info', message: 'b1', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-2' });

    // Error on t-1 should flush only t-1's buffer
    buffer.write({ level: 'error', message: 'err', timestamp: '2024-01-01T00:00:01.000Z', traceId: 't-1' });
    expect(delegate.entries).toHaveLength(2);
    expect(delegate.entries[0].traceId).toBe('t-1');
    expect(delegate.entries[1].traceId).toBe('t-1');

    buffer.close();
  });

  it('should flush remaining on close()', async () => {
    const buffer = new BufferSink({ sink: delegate, flushInterval: 60_000 });
    buffer.write({ level: 'info', message: 'pending', timestamp: '2024-01-01T00:00:00.000Z', traceId: 't-1' });
    await buffer.close();
    expect(delegate.entries).toHaveLength(1);
  });
});

describe('Logger -> JsonSink secret redaction (end to end)', () => {
  it('never writes a sensitive value to the JSON output', () => {
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };

    try {
      const logger = new Logger([new JsonSink()], 'db');
      logger.info('connecting', { host: 'localhost', password: 'hunter2', user: 'admin' });

      const line = output[0];
      // The raw secret must not appear anywhere in the emitted JSON line.
      expect(line).not.toContain('hunter2');
      const parsed = JSON.parse(line);
      expect(parsed.password).toBe('***');
      expect(parsed.host).toBe('localhost');
      expect(parsed.user).toBe('admin');
    } finally {
      console.log = originalLog;
    }
  });

  it('redacts sensitive fields carried on a thrown Error', () => {
    // Thrown errors often carry custom enumerable fields
    // (a driver error's connection config, an HTTP error's request headers).
    // toErrorEntry must redact them through the shared denylist so they never
    // reach a sink in plaintext.
    const output: string[] = [];
    const originalLog = console.log;
    console.log = (...args: unknown[]) => {
      output.push(args.join(' '));
    };

    try {
      const err = new Error('connection failed') as Error & {
        password?: string;
        config?: Record<string, unknown>;
        request?: Record<string, unknown>;
      };
      err.password = 'hunter2';
      err.config = { host: 'db.internal', password: 'nested-secret' };
      err.request = { headers: { authorization: 'Bearer leaked-token' } };

      const logger = new Logger([new JsonSink()], 'db');
      logger.error('query failed', err);

      const line = output[0];
      // No raw secret may appear anywhere in the emitted JSON line.
      expect(line).not.toContain('hunter2');
      expect(line).not.toContain('nested-secret');
      expect(line).not.toContain('leaked-token');

      const parsed = JSON.parse(line);
      // The serialized error keeps its standard shape...
      expect(parsed.error.name).toBe('Error');
      expect(parsed.error.message).toBe('connection failed');
      // ...and its custom fields are present but masked (top-level and nested).
      expect(parsed.error.password).toBe('***');
      expect(parsed.error.config.host).toBe('db.internal');
      expect(parsed.error.config.password).toBe('***');
      expect(parsed.error.request.headers.authorization).toBe('***');
    } finally {
      console.log = originalLog;
    }
  });
});
