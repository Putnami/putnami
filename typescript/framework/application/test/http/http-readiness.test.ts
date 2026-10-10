import { afterEach, describe, expect, it } from 'bun:test';
import { buildJsonRecord, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { READY_LOG_KEY, readyEndpointUrl, readyMarkerFromLogRecord } from '@putnami/runtime/jobs';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger } from '@putnami/runtime/testing';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { http, type HttpPlugin, listeningRecord } from '../../src/http/http.plugin';

/**
 * The TypeScript half of first-party readiness emission.
 *
 * A served app's stdout is a LOG stream — `@putnami/typescript`'s serve wrapper
 * re-emits each line as a job log event — so the framework announces readiness
 * by attaching a reserved machine-readable marker to the listening log record it
 * already writes. The extension's shared forwarder
 * (`tooling/extension-sdk/jsonl/forward.go`) turns that marker into the typed
 * `ready` runtime event. Its Go twin is
 * `go/framework/http/server_ready_test.go`.
 *
 * Assertions go through `buildJsonRecord` — the exact record the production
 * JSON sink prints — because that flattened record, not the LogEntry, is what
 * the forwarder actually parses.
 */

let app: Application | undefined;

afterEach(async () => {
  await app?.stop();
  app = undefined;
  resetDefaultLogger();
});

/** An application with one HTTP server, started, and its log records. */
async function startHttpApplication(): Promise<{
  plugin: HttpPlugin;
  record: (message: string) => Record<string, unknown>;
}> {
  const memory = new MemoryLogger();
  setRootLogger(memory);

  const plugin: HttpPlugin = http({ port: 0 }); // ephemeral: never collide with a real service
  app = application().use(plugin);
  await app.start();

  return {
    plugin,
    record: (message) => {
      const entry = memory.entries.find((e) => e.message.includes(message));
      if (!entry) {
        throw new Error(`no ${message} record among ${memory.entries.length} entries`);
      }
      return buildJsonRecord(entry);
    },
  };
}

async function startAndCaptureListeningRecord(): Promise<Record<string, unknown>> {
  return (await startHttpApplication()).record('listening http://');
}

describe('http readiness marker', () => {
  it('attaches a typed readiness payload to the listening log record', async () => {
    const record = await startAndCaptureListeningRecord();

    const data = readyMarkerFromLogRecord(record);
    expect(data).not.toBeNull();
    expect(data?.target).toBe('server');

    const endpoints = data?.endpoints ?? [];
    expect(endpoints).toHaveLength(1);
    for (const endpoint of endpoints) {
      expect(endpoint.scheme).toBe('http');
      expect(endpoint.host).toBe('localhost');
      // port 0 asks the kernel for an ephemeral port: the marker must carry the
      // BOUND port, or a consumer would be handed an address nothing listens on.
      expect(endpoint.port).toBeGreaterThan(0);
      expect(readyEndpointUrl(endpoint)).toBe(`http://localhost:${endpoint.port}`);
    }
    expect(JSON.stringify(record[READY_LOG_KEY])).not.toContain('"url"');
  });

  it('keeps the listening log human-readable', async () => {
    const record = await startAndCaptureListeningRecord();

    // Typing readiness must not cost a developer the line they actually read.
    // The CLI's log-substring probe is gone, so nothing machine-facing
    // depends on this message any more — which is precisely why
    // it needs a guard of its own now, rather than none.
    expect(String(record['message'])).toContain('listening http://');
    expect(record['severity']).toBe('INFO');
    expect(record['durationMs']).toBeNumber();
  });

  it('places the marker at the top level of the record the forwarder reads', async () => {
    const record = await startAndCaptureListeningRecord();

    // The forwarder looks for exactly one reserved top-level member; nesting it
    // under `data` (the sink's fallback for non-object payloads) would make it
    // invisible and silently drop typed readiness.
    expect(record[READY_LOG_KEY]).toBeDefined();
    expect(record['data']).toBeUndefined();
  });

  it('writes no marker for a listener that is not addressable', () => {
    for (const port of [undefined, 0, 65_536, 1.5]) {
      const record = listeningRecord(port, 12);
      expect(record[READY_LOG_KEY]).toBeUndefined();
      expect(record).toEqual({ durationMs: 12 });
    }
  });

  it('reports the bound endpoint from start until stop', async () => {
    expect(http({ port: 0 }).readyEndpoints()).toEqual([]);
    const { plugin, record } = await startHttpApplication();
    const claim = readyMarkerFromLogRecord(record('listening http://'));
    expect(claim?.endpoints).toHaveLength(1);
    expect(plugin.readyEndpoints()).toEqual(claim?.endpoints ?? []);

    await app?.stop();
    app = undefined;
    expect(plugin.readyEndpoints()).toEqual([]);
  });

  specTest(
    'puts the bound endpoint in the application ready record',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'completed-startup-readiness',
      check: 'the-ready-record-carries-the-http-endpoint',
    },
    async () => {
      const { record } = await startHttpApplication();

      const server = readyMarkerFromLogRecord(record('listening http://'));
      const workload = readyMarkerFromLogRecord(record('🤖 ready'));
      expect(server?.target).toBe('server');
      expect(workload?.target).toBe('workload');
      expect(workload?.endpoints).toHaveLength(1);
      expect(workload?.endpoints).toEqual(server?.endpoints ?? []);
    },
  );
});
