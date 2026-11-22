import { describe, expect } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { SQL } from 'bun';
import { QueryAbortError } from '../src/abort';
import { closeDatabase } from '../src/factory';
import { DATABASE_PROTOCOL_VERSION, setDatabaseBindingOverride } from '../src/postgres/binding';
import { __setPhysicalPoolFactoryForTests, driverOptions } from '../src/postgres/physical-pool';
import { DatabaseSessionStore } from '../src/session/database.store';

const DATASOURCE = 'session_abort';

/** A store whose table already exists, so it runs no DDL before its query. */
class ReadyStore extends DatabaseSessionStore {
  override async ensureTable(): Promise<void> {}
}

describe('DatabaseSessionStore under an ended request', () => {
  specTest(
    'writes nothing once the request is aborted',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'cancellation',
      check: 'the-session-store-stops-with-the-request',
    },
    async () => {
      // The real driver on a port nothing listens on: a query that started
      // would fail with a connection error, and a cancel would reject it
      // unread. Only the pool's opening ping answers.
      __setPhysicalPoolFactoryForTests((options) => {
        const real = new SQL(driverOptions({ ...options, host: '127.0.0.1', port: 1, max: 1, connectTimeout: 1 }));
        return new Proxy(real, {
          apply(target, thisArg, args: unknown[]) {
            const strings = args[0];
            if (Array.isArray(strings) && strings.join('').trim() === 'SELECT 1') return Promise.resolve([{ ok: 1 }]);
            return Reflect.apply(target, thisArg, args);
          },
        });
      });
      const previous = setDatabaseBindingOverride(
        JSON.stringify({
          protocolVersion: DATABASE_PROTOCOL_VERSION,
          databases: {
            [DATASOURCE]: {
              engine: 'postgres',
              schema: DATASOURCE,
              connection: { host: '127.0.0.1', port: 1, database: DATASOURCE, user: 'u', password: 'p', ssl: false },
            },
          },
        }),
      );
      const unhandled: unknown[] = [];
      const onUnhandled = (reason: unknown) => unhandled.push(reason);
      process.on('unhandledRejection', onUnhandled);
      try {
        const store = new ReadyStore(DATASOURCE);
        const controller = new AbortController();
        controller.abort(new DOMException('Request Timeout', 'TimeoutError'));
        const steps: Array<() => Promise<unknown>> = [
          () => store.setAllAsync('aborted-session', { userId: 'user-123' }),
          () => store.getAllAsync('aborted-session'),
          () => store.deleteAllAsync('aborted-session'),
        ];
        for (const step of steps) {
          const outcome = await Promise.resolve(runInContext({ signal: controller.signal }, step)).catch(
            (error: unknown) => error,
          );
          expect(outcome).toBeInstanceOf(QueryAbortError);
        }
        await new Promise((resolve) => setTimeout(resolve, 50));
        expect(unhandled).toEqual([]);
      } finally {
        process.off('unhandledRejection', onUnhandled);
        await closeDatabase(DATASOURCE);
        setDatabaseBindingOverride(previous);
        __setPhysicalPoolFactoryForTests(undefined);
      }
    },
  );
});
