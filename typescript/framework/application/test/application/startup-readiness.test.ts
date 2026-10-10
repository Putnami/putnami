import { afterEach, describe, expect } from 'bun:test';
import { buildJsonRecord, type LogEntry, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { READY_LOG_KEY, type ReadyEndpoint, readyMarkerFromLogRecord } from '@putnami/runtime/jobs';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger } from '@putnami/runtime/testing';
import { type Application, application, type Plugin } from '../../src/application';

/**
 * The ready record is the application's own readiness claim: `start()` writes
 * it, with a workload marker under the reserved key the serve forwarder reads,
 * only once every plugin `start()` resolved. Its Go twin is
 * `go/framework/app/startup_readiness_test.go`.
 *
 * Assertions read the flattened record `buildJsonRecord` produces, which is
 * what the production JSON sink prints and the forwarder parses.
 */

const HANG_DETECTOR_MS = 30_000;

let app: Application | undefined;
let memory: MemoryLogger;

afterEach(async () => {
  await app?.stop();
  app = undefined;
  resetDefaultLogger();
});

function captureLogs(): MemoryLogger {
  memory = new MemoryLogger();
  setRootLogger(memory);
  return memory;
}

/** Every record that carries a readiness marker, of any target. */
function markedRecords(): Record<string, unknown>[] {
  return memory.entries
    .map((entry: LogEntry) => buildJsonRecord(entry))
    .filter((record) => record[READY_LOG_KEY] !== undefined);
}

function hasWorkloadMarker(): boolean {
  return markedRecords().some((record) => readyMarkerFromLogRecord(record)?.target === 'workload');
}

/** A promise with its resolver, for a plugin start the test releases. */
function gate(): { promise: Promise<void>; release: () => void } {
  let release = (): void => {};
  const promise = new Promise<void>((resolve) => {
    release = resolve;
  });
  return { promise, release };
}

async function withinHangDetector<T>(promise: Promise<T>, what: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} did not happen`)), HANG_DETECTOR_MS);
  });
  try {
    return await Promise.race([promise, timeout]);
  } finally {
    clearTimeout(timer);
  }
}

describe('ready record', () => {
  specTest(
    'follows every plugin start',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'completed-startup-readiness',
      check: 'the-ready-record-follows-every-plugin-start',
    },
    async () => {
      captureLogs();
      const markedWhenStarted: boolean[] = [];
      const starter = (): Plugin => ({
        start: async () => {
          await Promise.resolve();
          markedWhenStarted.push(hasWorkloadMarker());
        },
      });
      app = application().use(starter()).use(starter());
      const order: string[] = [];
      app.run(async () => {
        order.push(hasWorkloadMarker() ? 'runner-after-ready' : 'runner-before-ready');
      });

      await app.start();

      expect(markedWhenStarted).toEqual([false, false]);
      expect(order).toEqual(['runner-after-ready']);
      const marked = markedRecords();
      expect(marked).toHaveLength(1);
      const record = marked[0] ?? {};
      expect(record['message']).toBe('🤖 ready');
      expect(record['durationMs']).toBeNumber();
      const data = readyMarkerFromLogRecord(record);
      expect(data?.target).toBe('workload');
      expect(data?.endpoints ?? []).toHaveLength(0);
    },
  );

  specTest(
    'waits for a delayed plugin start',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'completed-startup-readiness',
      check: 'a-delayed-plugin-start-delays-the-ready-record',
    },
    async () => {
      captureLogs();
      const slow = gate();
      const entered = gate();
      let completed = false;
      app = application()
        .use({ start: async () => {} } satisfies Plugin)
        .use({
          startupCompleted: () => {
            completed = true;
          },
        } satisfies Plugin)
        .use({
          start: async () => {
            entered.release();
            await slow.promise;
          },
        } satisfies Plugin);

      const started = app.start();
      await withinHangDetector(entered.promise, 'the delayed start');
      // Let every other pending start settle: only the gated one may hold the record back.
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(hasWorkloadMarker()).toBe(false);
      expect(completed).toBe(false);

      slow.release();
      await withinHangDetector(started, 'start()');
      expect(hasWorkloadMarker()).toBe(true);
      expect(completed).toBe(true);
    },
  );

  specTest(
    'is not written when a plugin start rejects',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'completed-startup-readiness',
      check: 'a-rejected-plugin-start-writes-no-ready-record',
    },
    async () => {
      captureLogs();
      let ran = false;
      let completed = false;
      app = application()
        .use({ start: async () => {} } satisfies Plugin)
        .use({
          startupCompleted: () => {
            completed = true;
          },
        } satisfies Plugin)
        .use({
          start: async () => {
            throw new Error('boom');
          },
        } satisfies Plugin);
      app.run(async () => {
        ran = true;
      });

      await expect(app.start()).rejects.toThrow('boom');

      expect(completed).toBe(false);
      expect(markedRecords()).toHaveLength(0);
      expect(memory.entries.some((entry) => entry.message.includes('🤖 ready'))).toBe(false);
      expect(ran).toBe(false);
    },
  );

  specTest(
    'follows the plugins learning that startup completed',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'completed-startup-readiness',
      check: 'plugins-learn-completed-startup-before-the-ready-record',
    },
    async () => {
      captureLogs();
      const events: string[] = [];
      const observer = (name: string): Plugin => ({
        start: async () => {
          await Promise.resolve();
          events.push(`start:${name}`);
        },
        startupCompleted: () => {
          events.push(hasWorkloadMarker() ? `late:${name}` : `completed:${name}`);
        },
      });
      app = application().use(observer('alpha')).use(observer('beta'));
      app.run(async () => {
        events.push('runner');
      });

      await app.start();

      expect(events).toEqual(['start:alpha', 'start:beta', 'completed:alpha', 'completed:beta', 'runner']);
      expect(hasWorkloadMarker()).toBe(true);
    },
  );

  specTest(
    'carries the endpoints the plugins bound',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'completed-startup-readiness',
      check: 'the-ready-record-carries-the-endpoints-the-plugins-bound',
    },
    async () => {
      captureLogs();
      const http = (port: number): ReadyEndpoint => ({ scheme: 'http', host: 'localhost', port });
      const grpc: ReadyEndpoint = { scheme: 'grpc', host: 'localhost', port: 9090 };
      const admin = [http(8081), http(8080)];
      app = application()
        .use({ readyEndpoints: () => admin } satisfies Plugin)
        .use({ readyEndpoints: () => [http(8080), grpc] } satisfies Plugin)
        .use({ readyEndpoints: () => [] } satisfies Plugin)
        // An endpoint the protocol rejects is dropped; kept, it would void the claim.
        .use({ readyEndpoints: () => [http(0)] } satisfies Plugin);

      await app.start();

      const marked = markedRecords();
      expect(marked).toHaveLength(1);
      const data = readyMarkerFromLogRecord(marked[0] ?? {});
      expect(data?.target).toBe('workload');
      expect(data?.endpoints).toEqual([grpc, http(8080), http(8081)]);
      expect(admin).toEqual([http(8081), http(8080)]);
    },
  );
});

describe('markAsRunning', () => {
  specTest(
    'tells every plugin that startup completed',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'completed-startup-readiness',
      check: 'plugins-learn-completed-startup-before-the-ready-record',
    },
    () => {
      const completed: string[] = [];
      app = application()
        .use({ startupCompleted: () => completed.push('alpha') } satisfies Plugin)
        .use({ startupCompleted: () => completed.push('beta') } satisfies Plugin);

      app.markAsRunning();

      expect(app.isRunning()).toBe(true);
      expect(completed).toEqual(['alpha', 'beta']);
    },
  );
});
