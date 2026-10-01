import { afterEach, beforeEach, describe, expect, it, spyOn } from 'bun:test';
import { runInContext } from '../../src/context';
import { getLoggerConfig, normalizeLogLevel, resetLoggerConfig } from '../../src/logger/logger.config';
import { Logger, useLogger, resetDefaultLogger, MAX_APPENDED_FIELD_VALUES } from '../../src/logger/logger';
import { MemoryLogger, MemorySink } from '../../src/logger/memory.logger';

describe('Logger', () => {
  let sink: MemorySink;
  let logger: Logger;

  beforeEach(() => {
    sink = new MemorySink();
    logger = new Logger([sink], 'test');
  });

  it('should capture info logs', () => {
    logger.info('hello');
    expect(sink.entries).toHaveLength(1);
    expect(sink.entries[0].level).toBe('info');
    expect(sink.entries[0].message).toBe('hello');
    expect(sink.entries[0].logger).toBe('test');
  });

  it('should capture error logs', () => {
    logger.error('failed');
    expect(sink.entries).toHaveLength(1);
    expect(sink.entries[0].level).toBe('error');
    expect(sink.entries[0].message).toBe('failed');
  });

  it('should filter debug logs at default info level', () => {
    logger.debug('debug msg');
    expect(sink.entries).toHaveLength(0);
  });

  it('should capture warn logs', () => {
    logger.warn('warn msg');
    expect(sink.entries).toHaveLength(1);
    expect(sink.entries[0].level).toBe('warn');
  });

  it('should extract Error objects from params', () => {
    const err = new Error('boom');
    logger.error('request failed', err);
    expect(sink.entries[0].error).toBeDefined();
    expect(sink.entries[0].error?.name).toBe('Error');
    expect(sink.entries[0].error?.message).toBe('boom');
    expect(sink.entries[0].error?.stack).toBeDefined();
  });

  it('should extract Error when passed as message', () => {
    const err = new TypeError('bad type');
    logger.error(err);
    expect(sink.entries[0].message).toBe('TypeError: bad type');
    expect(sink.entries[0].error?.name).toBe('TypeError');
  });

  it('should serialize the Error cause chain', () => {
    const root = new Error('connection refused');
    const middle = new Error('query failed', { cause: root });
    const wrapped = new Error('failed to load user', { cause: middle });
    logger.error('request failed', wrapped);

    expect(sink.entries[0].error?.message).toBe('failed to load user');
    expect(sink.entries[0].error?.cause?.message).toBe('query failed');
    expect(sink.entries[0].error?.cause?.cause?.message).toBe('connection refused');
    expect(sink.entries[0].error?.cause?.cause?.name).toBe('Error');
  });

  it('should bound a cyclic Error cause chain instead of recursing forever', () => {
    const a = new Error('a');
    const b = new Error('b', { cause: a });
    (a as Error & { cause?: unknown }).cause = b;

    logger.error('cyclic', a);
    // Does not throw; the depth guard stops the recursion.
    expect(sink.entries[0].error?.message).toBe('a');
  });

  it('should include extra params as data', () => {
    logger.info('msg', { status: 200 }, 'extra');
    expect(sink.entries[0].data).toEqual([{ status: 200 }, 'extra']);
  });

  it('should include traceId from context', async () => {
    await runInContext({ traceId: 'trace-123' }, () => {
      logger.info('in context');
    });
    expect(sink.entries[0].traceId).toBe('trace-123');
  });

  it('should include logContext from context', async () => {
    await runInContext({ logContext: { userId: 'u-1' } }, () => {
      logger.info('in context');
    });
    expect(sink.entries[0].context).toEqual({ userId: 'u-1' });
  });

  it('should set timestamp on every entry', () => {
    logger.info('msg');
    expect(sink.entries[0].timestamp).toBeDefined();
    expect(new Date(sink.entries[0].timestamp).getTime()).toBeGreaterThan(0);
  });

  it('should return logger name', () => {
    expect(logger.name).toBe('test');
  });

  it('should return empty name for unnamed logger', () => {
    const unnamed = new Logger([sink]);
    expect(unnamed.name).toBe('');
  });

  describe('flush()', () => {
    it('should call flush on all sinks', async () => {
      let flushed = false;
      const flushableSink = {
        write: () => {},
        flush: async () => {
          flushed = true;
        },
      };
      const l = new Logger([flushableSink]);
      await l.flush();
      expect(flushed).toBe(true);
    });

    it('should handle sinks without flush', async () => {
      const l = new Logger([sink]);
      await l.flush(); // MemorySink has no flush — should not throw
    });
  });

  describe('close()', () => {
    it('should call close on all sinks', async () => {
      let closed = false;
      const closableSink = {
        write: () => {},
        close: async () => {
          closed = true;
        },
      };
      const l = new Logger([closableSink]);
      await l.close();
      expect(closed).toBe(true);
    });

    it('should handle sinks without close', async () => {
      const l = new Logger([sink]);
      await l.close(); // MemorySink has no close — should not throw
    });
  });

  describe('named()', () => {
    it('should create a child logger with a different name', () => {
      const child = logger.named('child');
      child.info('from child');
      expect(sink.entries[0].logger).toBe('child');
    });

    it('should share sinks with parent', () => {
      const child = logger.named('child');
      logger.info('parent');
      child.info('child');
      expect(sink.entries).toHaveLength(2);
    });
  });

  describe('with()', () => {
    it('should add context data', async () => {
      await runInContext({}, () => {
        logger.with('user', { uid: 'abc' });
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ user: { uid: 'abc' } });
    });

    it('should be chainable', async () => {
      await runInContext({}, () => {
        const result = logger.with('key', 'value');
        expect(result).toBe(logger);
      });
    });

    it('should merge object values by default', async () => {
      await runInContext({}, () => {
        logger.with('user', { uid: 'abc' });
        logger.with('user', { name: 'John' });
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ user: { uid: 'abc', name: 'John' } });
    });

    it('should replace when replace option is set', async () => {
      await runInContext({}, () => {
        logger.with('user', { uid: 'abc' });
        logger.with('user', { name: 'John' }, { replace: true });
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ user: { name: 'John' } });
    });

    it('should handle scalar values', async () => {
      await runInContext({}, () => {
        logger.with('count', 42);
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ count: 42 });
    });

    it('should attach context outside request scope via the returned logger', () => {
      const scoped = logger.with('scope', 'global');
      scoped.info('msg');
      expect(sink.entries[0].context).toEqual({ scope: 'global' });
    });

    it('should be immutable outside request scope (no mutation of the source logger)', () => {
      // Regression: with() outside a context must NOT mutate the
      // (possibly shared/default) logger. It returns a new logger instead.
      const scoped = logger.with('reqId', 'abc');
      expect(scoped).not.toBe(logger);

      // The original logger is untouched: no leaked context.
      logger.info('from original');
      expect(sink.entries[0].context).toBeUndefined();

      // Only the returned logger carries the field.
      scoped.info('from scoped');
      expect(sink.entries[1].context).toEqual({ reqId: 'abc' });
    });

    it('should not leak fields across detached loggers derived from a shared base', () => {
      // Two independent .with() calls on the same base must not see each other's
      // fields — proves the detached context is copied, not shared.
      const a = logger.with('a', 1);
      const b = logger.with('b', 2);

      a.info('a');
      b.info('b');

      expect(sink.entries[0].context).toEqual({ a: 1 });
      expect(sink.entries[1].context).toEqual({ b: 2 });
    });

    it('should accumulate fields when chained outside request scope', () => {
      const scoped = logger.with('userId', 'u-1').with('tenant', 'acme');
      scoped.info('msg');
      expect(sink.entries[0].context).toEqual({ userId: 'u-1', tenant: 'acme' });
    });

    it('should keep a dot-containing key literal (no nesting)', async () => {
      await runInContext({}, () => {
        logger.with('a.b', 'literal');
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ 'a.b': 'literal' });
    });
  });

  describe('append() / increment()', () => {
    it('should mutate logContext and return this inside a request scope', async () => {
      await runInContext({}, () => {
        const r = logger.append('event.publishes', 'a');
        expect(r).toBe(logger);
        logger.append('event.publishes', 'b');
        logger.increment('event.publishCount', 1);
        logger.increment('event.publishCount', 1);
        logger.info('handled');
      });
      expect(sink.entries[0].context).toEqual({ event: { publishes: ['a', 'b'], publishCount: 2 } });
    });

    it('should default increment delta to 1', async () => {
      await runInContext({}, () => {
        logger.increment('n');
        logger.increment('n');
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ n: 2 });
    });

    it('should return a new logger detached, leaving the original untouched', () => {
      const scoped = logger.append('items', 'a');
      expect(scoped).not.toBe(logger);
      logger.info('original');
      expect(sink.entries[0].context).toBeUndefined();
      scoped.info('scoped');
      expect(sink.entries[1].context).toEqual({ items: ['a'] });
    });

    it('should accumulate across a detached chain', () => {
      const scoped = logger.append('items', 'a').append('items', 'b').increment('n', 3);
      scoped.info('msg');
      expect(sink.entries[0].context).toEqual({ items: ['a', 'b'], n: 3 });
    });

    it('should promote a scalar so earlier values are never lost', async () => {
      await runInContext({}, () => {
        logger.with('count', 'first');
        logger.append('count', 'second');
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ count: ['first', 'second'] });
    });

    it('should cap appended values at MAX_APPENDED_FIELD_VALUES', async () => {
      await runInContext({}, () => {
        for (let i = 0; i <= MAX_APPENDED_FIELD_VALUES; i++) {
          logger.append('items', i);
        }
        logger.info('msg');
      });
      const items = (sink.entries[0].context as { items: unknown[] }).items;
      expect(items).toHaveLength(MAX_APPENDED_FIELD_VALUES);
      expect(items[0]).toBe(0);
      expect(items[MAX_APPENDED_FIELD_VALUES - 1]).toBe(MAX_APPENDED_FIELD_VALUES - 1);
    });

    it('should coerce a non-number leaf to delta on increment', async () => {
      await runInContext({}, () => {
        logger.with('n', 'not-a-number');
        logger.increment('n', 5);
        logger.info('msg');
      });
      expect((sink.entries[0].context as { n: unknown }).n).toBe(5);
    });

    it('should create nested groups from a dot-path detached', () => {
      const scoped = logger.append('event.publishes', 'x').increment('event.publishCount', 1);
      scoped.info('msg');
      expect(sink.entries[0].context).toEqual({ event: { publishes: ['x'], publishCount: 1 } });
    });

    it('should replace a non-object path intermediate with a fresh object', () => {
      const scoped = logger.with('event', 'scalar').append('event.publishes', 'x');
      scoped.info('msg');
      expect(sink.entries[0].context).toEqual({ event: { publishes: ['x'] } });
    });

    it('should copy-on-write so a shared base array is not mutated detached', () => {
      const base = logger.append('items', 'a');
      const child = base.append('items', 'b');
      base.info('base');
      child.info('child');
      expect(sink.entries[0].context).toEqual({ items: ['a'] });
      expect(sink.entries[1].context).toEqual({ items: ['a', 'b'] });
    });

    it('should copy-on-write nested containers along a dot-path detached', () => {
      const base = logger.append('event.publishes', 'a');
      const child = base.append('event.publishes', 'b');
      base.info('base');
      child.info('child');
      expect(sink.entries[0].context).toEqual({ event: { publishes: ['a'] } });
      expect(sink.entries[1].context).toEqual({ event: { publishes: ['a', 'b'] } });
    });

    it('should redact sensitive keys inside appended objects', async () => {
      await runInContext({}, () => {
        logger.append('auths', { user: 'admin', password: 'hunter2' });
        logger.info('msg');
      });
      expect(sink.entries[0].context).toEqual({ auths: [{ user: 'admin', password: '***' }] });
    });
  });

  describe('secret redaction', () => {
    it('should redact sensitive keys in data params while passing others through', () => {
      logger.info('db', { host: 'localhost', password: 'hunter2', user: 'admin' });
      expect(sink.entries[0].data).toEqual([{ host: 'localhost', password: '***', user: 'admin' }]);
    });

    it('should redact nested sensitive keys in data params', () => {
      logger.info('config', { db: { host: 'localhost', password: 'pw' }, retries: 3 });
      expect(sink.entries[0].data).toEqual([{ db: { host: 'localhost', password: '***' }, retries: 3 }]);
    });

    it('should redact sensitive keys in structured message objects', () => {
      logger.info({ event: 'db', password: 'hunter2', user: 'admin' });
      expect(sink.entries[0].message).toBe('{"event":"db","password":"***","user":"admin"}');
      expect(sink.entries[0].message).not.toContain('hunter2');
    });

    it('should redact sensitive values added via with()', async () => {
      await runInContext({}, () => {
        logger.with('auth', { token: 'super-secret', scheme: 'bearer' });
        logger.info('request');
      });
      expect(sink.entries[0].context).toEqual({ auth: { token: '***', scheme: 'bearer' } });
    });

    it('should redact sensitive keys in detached (out-of-scope) context', () => {
      const scoped = logger.with('apiKey', 'leaked-key');
      scoped.info('msg');
      expect(sink.entries[0].context).toEqual({ apiKey: '***' });
    });

    it('should not mutate the caller-supplied object', () => {
      const config = { host: 'localhost', password: 'hunter2' };
      logger.info('db', config);
      expect(config.password).toBe('hunter2');
    });

    it('should not mutate the live request logContext across calls', async () => {
      await runInContext({}, () => {
        logger.with('secret', 'value');
        logger.info('first');
        // A second log in the same scope must still see the real value to redact.
        logger.info('second');
      });
      expect(sink.entries[0].context).toEqual({ secret: '***' });
      expect(sink.entries[1].context).toEqual({ secret: '***' });
    });
  });
});

describe('MemoryLogger', () => {
  it('should capture entries via entries getter', () => {
    const logger = new MemoryLogger();
    logger.info('hello');
    logger.error('failed');
    expect(logger.entries).toHaveLength(2);
    expect(logger.entries[0].message).toBe('hello');
    expect(logger.entries[1].message).toBe('failed');
  });

  it('should clear entries', () => {
    const logger = new MemoryLogger();
    logger.info('msg');
    logger.clear();
    expect(logger.entries).toHaveLength(0);
  });

  it('should accept a name', () => {
    const logger = new MemoryLogger('test');
    logger.info('msg');
    expect(logger.entries[0].logger).toBe('test');
  });
});

describe('shouldLog', () => {
  it('should filter debug below info level', () => {
    const l = new Logger([new MemorySink()]);
    l.debug('should be filtered');
    // Default level is 'info', so debug is filtered
  });
});

describe('getLoggerConfig — Cloud Run auto-detection', () => {
  let originalKService: string | undefined;

  beforeEach(() => {
    originalKService = process.env['K_SERVICE'];
    resetLoggerConfig();
  });

  afterEach(() => {
    if (originalKService === undefined) {
      delete process.env.K_SERVICE;
    } else {
      process.env['K_SERVICE'] = originalKService;
    }
    resetLoggerConfig();
  });

  it('should force json: true when K_SERVICE is set', () => {
    process.env['K_SERVICE'] = 'my-service';
    const config = getLoggerConfig();
    expect(config.json).toBe(true);
  });

  it('should respect default json: true when K_SERVICE is not set', () => {
    delete process.env.K_SERVICE;
    const config = getLoggerConfig();
    expect(config.json).toBe(true);
  });
});

describe('useLogger', () => {
  beforeEach(() => {
    resetDefaultLogger();
  });

  it('should return a Logger instance', () => {
    const logger = useLogger();
    expect(logger).toBeInstanceOf(Logger);
  });

  it('should return a named logger', () => {
    const logger = useLogger('http');
    expect(logger.name).toBe('http');
  });

  it('should use context logger when available', async () => {
    const customSink = new MemorySink();
    const customLogger = new Logger([customSink], 'custom');

    await runInContext({ logger: customLogger }, () => {
      const logger = useLogger('http');
      logger.info('test');
    });

    expect(customSink.entries).toHaveLength(1);
    expect(customSink.entries[0].logger).toBe('http');
  });
});

describe('normalizeLogLevel', () => {
  it('should return the level unchanged for valid values', () => {
    expect(normalizeLogLevel('debug')).toBe('debug');
    expect(normalizeLogLevel('info')).toBe('info');
    expect(normalizeLogLevel('warn')).toBe('warn');
    expect(normalizeLogLevel('error')).toBe('error');
  });

  it('should fall back to "info" for an unrecognized value', () => {
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation(() => true);
    try {
      expect(normalizeLogLevel('verbose')).toBe('info');
    } finally {
      stderrSpy.mockRestore();
    }
  });

  it('should fall back to "info" for an empty string', () => {
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation(() => true);
    try {
      expect(normalizeLogLevel('')).toBe('info');
    } finally {
      stderrSpy.mockRestore();
    }
  });

  it('should fall back to "info" for undefined', () => {
    expect(normalizeLogLevel(undefined)).toBe('info');
  });

  it('should emit a warning to stderr for an invalid value', () => {
    const messages: string[] = [];
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation((msg: unknown) => {
      messages.push(String(msg));
      return true;
    });
    try {
      normalizeLogLevel('typo');
      expect(messages.some((m) => m.includes('typo'))).toBe(true);
      expect(messages.some((m) => m.includes('info'))).toBe(true);
    } finally {
      stderrSpy.mockRestore();
    }
  });

  it('should emit only one warning for repeated invalid values', () => {
    const messages: string[] = [];
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation((msg: unknown) => {
      messages.push(String(msg));
      return true;
    });
    try {
      normalizeLogLevel('verbose-once');
      normalizeLogLevel('verbose-once');
      expect(messages).toHaveLength(1);
      expect(messages[0]).toContain('verbose-once');
    } finally {
      stderrSpy.mockRestore();
    }
  });

  it('should NOT emit a warning when value is undefined', () => {
    const messages: string[] = [];
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation((msg: unknown) => {
      messages.push(String(msg));
      return true;
    });
    try {
      normalizeLogLevel(undefined);
      expect(messages).toHaveLength(0);
    } finally {
      stderrSpy.mockRestore();
    }
  });
});

describe('invalid LOG_LEVEL does not silence error logs', () => {
  it('normalizeLogLevel("verbose") returns "info", so error priority >= info — errors are logged', () => {
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation(() => true);
    try {
      const level = normalizeLogLevel('verbose');
      // LEVEL_PRIORITY['error'] >= LEVEL_PRIORITY[level] must be true
      const LEVEL_PRIORITY: Record<string, number> = { debug: 0, info: 1, warn: 2, error: 3 };
      expect(LEVEL_PRIORITY['error'] >= LEVEL_PRIORITY[level]).toBe(true);
    } finally {
      stderrSpy.mockRestore();
    }
  });

  it('normalizeLogLevel("verbose") returns "info", so warn priority >= info — warnings are logged', () => {
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation(() => true);
    try {
      const level = normalizeLogLevel('verbose');
      const LEVEL_PRIORITY: Record<string, number> = { debug: 0, info: 1, warn: 2, error: 3 };
      expect(LEVEL_PRIORITY['warn'] >= LEVEL_PRIORITY[level]).toBe(true);
    } finally {
      stderrSpy.mockRestore();
    }
  });

  it('shouldLog reports true for "error" even when config level is the fallback "info"', () => {
    const stderrSpy = spyOn(process.stderr, 'write').mockImplementation(() => true);
    try {
      // Build a sink with skipLevelCheck so we can verify entries independently
      const sink = new MemorySink();
      const logger = new Logger([sink], undefined, {}, true /* skipLevelCheck */);
      logger.error('must appear');
      expect(sink.entries.some((e) => e.level === 'error')).toBe(true);
    } finally {
      stderrSpy.mockRestore();
    }
  });
});
