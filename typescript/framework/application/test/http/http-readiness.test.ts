import { afterEach, describe, expect, it } from 'bun:test';
import { buildJsonRecord, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { READY_LOG_KEY, readyEndpointUrl, readyMarkerFromLogRecord } from '@putnami/runtime/jobs';
import { MemoryLogger } from '@putnami/runtime/testing';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { http, type HttpPlugin } from '../../src/http/http.plugin';

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

async function startAndCaptureListeningRecord(): Promise<Record<string, unknown>> {
  const memory = new MemoryLogger();
  setRootLogger(memory);

  const plugin: HttpPlugin = http({ port: 0 }); // ephemeral: never collide with a real service
  app = application().use(plugin);
  await app.start();

  const entry = memory.entries.find((e) => e.message.includes('listening http://'));
  if (!entry) {
    throw new Error(`no listening record among ${memory.entries.length} entries`);
  }
  return buildJsonRecord(entry);
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
});
