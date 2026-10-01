import { afterEach, describe, expect, it } from 'bun:test';
import { resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { assertRecord, findCase, findRecord, loadCases, MemoryLogger } from '@putnami/runtime/testing';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { HttpResponse } from '../../src/http/http-response';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { logger as loggerPlugin } from '../../src/http/logger.plugin';
import { trace } from '../../src/http/trace.plugin';

/**
 * This file executes the HTTP boundary's cases of the canonical cross-runtime log
 * corpus (`protocols/logging/conformance`) against the records the REAL middleware
 * chain emits through the REAL JSON sink path (`buildJsonRecord`). Its Go twin is
 * `go/framework/http/logging_cross_language_test.go`; a divergence here means
 * either the change forgot the other runtime or it forgot the corpus.
 *
 * The accumulation case (`http.terminal.success-with-publishes`) is driven from
 * `typescript/framework/events/test/logging-cross-language.test.ts`: publishing is
 * what accumulates onto this boundary's terminal record, and `@putnami/events` is
 * the package that depends on this one (never the reverse).
 */

const cases = loadCases('http');
const ROUTE = '/conformance/orders';

let app: Application | undefined;

afterEach(async () => {
  await app?.stop();
  app = undefined;
  resetDefaultLogger();
});

/**
 * Start an app with the production logging wiring — `trace()` (the twin of Go's
 * `RequestID()`, which seeds the terminal record's traceId) plus `logger()` (the
 * real LoggerMiddleware) — drive one request through the real dispatcher, and
 * return the captured entries.
 */
async function driveRequest(configure: (plugin: HttpPlugin) => void, method = 'GET'): Promise<MemoryLogger> {
  const memory = new MemoryLogger();
  setRootLogger(memory);

  const plugin = http({ port: 0 });
  configure(plugin);
  app = application().use(plugin).use(trace()).use(loggerPlugin());
  await app.start();

  const res = await fetch(`http://localhost:${plugin.getServer()?.port}${ROUTE}`, { method });
  // Draining the body guarantees the response (and therefore the terminal record)
  // is complete before the entries are read.
  await res.text();
  return memory;
}

describe('http logging conformance', () => {
  it('emits the corpus record for a 2xx response', async () => {
    const want = findCase(cases, 'http.terminal.success');

    const memory = await driveRequest((plugin) => {
      plugin.get(ROUTE, () => HttpResponse.json({ ok: true }));
    });

    assertRecord(findRecord(memory.entries, want), want);
  });

  it('emits the corpus record for a handler failure mapped to 500', async () => {
    const want = findCase(cases, 'http.terminal.5xx');

    const memory = await driveRequest((plugin) => {
      plugin.get(ROUTE, () => {
        // The dispatcher records this on the context as `__requestError`, and the
        // terminal record reads it back — the message stays a stable constant.
        throw new Error('boom');
      });
    });

    const terminal = findRecord(memory.entries, want);
    assertRecord(terminal, want);
    expect(terminal.message).not.toContain('boom');
  });
});
