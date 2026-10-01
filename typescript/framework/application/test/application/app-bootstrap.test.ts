import { afterEach, beforeEach, describe, expect, spyOn } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import { application } from '../../src/application';
import { bootstrapServe } from '../../src/application/app-bootstrap';

describe('bootstrapServe', () => {
  let logger: MemoryLogger;
  const restores: Array<() => void> = [];

  beforeEach(() => {
    // Route the process-wide logger to a capturing sink. Both start()'s own
    // catch and the bootstrap guard resolve `useLogger('putnami')`, which shares
    // this root's sink, so every error line lands in `logger.entries`.
    logger = new MemoryLogger();
    setRootLogger(logger);
  });

  afterEach(() => {
    resetDefaultLogger();
    for (const restore of restores.splice(0)) {
      restore();
    }
  });

  function spyExit() {
    const spy = spyOn(process, 'exit').mockImplementation((() => undefined) as never);
    restores.push(() => spy.mockRestore());
    return spy;
  }

  const startupErrors = () => logger.entries.filter((e) => e.level === 'error' && e.message.includes('startup failed'));

  specTest(
    'starts the app and neither logs nor exits on success',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'bootstrap-guard',
      check: 'a-successful-bootstrap-neither-logs-nor-exits',
    },
    async () => {
      const exit = spyExit();
      const app = application();
      let started = false;
      const startSpy = spyOn(app, 'start').mockImplementation(async () => {
        started = true;
      });
      restores.push(() => startSpy.mockRestore());

      await bootstrapServe(() => app);

      expect(started).toBe(true);
      expect(exit).not.toHaveBeenCalled();
      expect(startupErrors()).toHaveLength(0);
    },
  );

  specTest(
    'logs exactly one structured error and exits non-zero when construction throws',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'bootstrap-guard',
      check: 'a-construction-time-failure-logs-once-and-exits-non-zero',
    },
    async () => {
      const exit = spyExit();
      const boom = new Error('config-server returned non-200: status 404');

      await bootstrapServe(() => {
        throw boom;
      });

      const errors = startupErrors();
      expect(errors).toHaveLength(1);
      expect(errors[0].message).toContain('config-server returned non-200: status 404');
      // The thrown error is captured as a structured field — one JSON line, not a
      // multi-line stack dump.
      expect(errors[0].error).toBeDefined();
      expect(exit).toHaveBeenCalledWith(1);
    },
  );

  specTest(
    'handles an async factory rejection',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'bootstrap-guard',
      check: 'an-async-factory-rejection-is-reported-the-same-way',
    },
    async () => {
      const exit = spyExit();

      await bootstrapServe(() => Promise.reject(new Error('async boom')));

      expect(startupErrors()).toHaveLength(1);
      expect(exit).toHaveBeenCalledWith(1);
    },
  );

  specTest(
    'does not double-log a start()-phase failure that start() already logged',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'bootstrap-guard',
      check: 'a-start-phase-failure-is-not-double-logged',
    },
    async () => {
      const exit = spyExit();
      const app = application();
      const boom = new Error('plugin start boom');
      // Real start() logs `‼️ startup failed`, tags the error, then re-throws it to
      // the guard. Drive that path by making prepare() reject.
      const prepareSpy = spyOn(app, 'prepare').mockImplementation(async () => {
        throw boom;
      });
      restores.push(() => prepareSpy.mockRestore());

      await bootstrapServe(() => app);

      // start()'s own line, and no duplicate from the guard.
      expect(startupErrors()).toHaveLength(1);
      expect(exit).toHaveBeenCalledWith(1);
    },
  );

  specTest(
    'does not double-log when start() re-throws a primitive value',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'bootstrap-guard',
      check: 'a-re-thrown-primitive-is-not-double-logged',
    },
    async () => {
      const exit = spyExit();
      const app = application();
      // A plugin/runner can reject with a non-object; start() logs+re-throws it and
      // the guard must still treat it as already logged (primitives can't live in a
      // WeakSet, so they need their own tracking).
      const prepareSpy = spyOn(app, 'prepare').mockImplementation(async () => {
        throw 'primitive boom';
      });
      restores.push(() => prepareSpy.mockRestore());

      await bootstrapServe(() => app);

      expect(startupErrors()).toHaveLength(1);
      expect(startupErrors()[0].message).toContain('primitive boom');
      expect(exit).toHaveBeenCalledWith(1);
    },
  );
});
