import { describe, expect, it } from 'bun:test';
import { Optional } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import type { ClientOperationPolicy } from '../../src/api/client-contract';
import type { DiscoveredRoute } from '../../src/api/discovered-route.type';
import { generateOpenApiSpec, type OpenApiOptions } from '../../src/openapi/openapi';

const OPTIONS: OpenApiOptions = {
  info: { title: 'Logs', version: '1.0.0' },
  client: { service: { id: 'logs', audience: 'api://logs' }, credentials: {} },
};

const CURSOR = { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } } as const;

function logRoute(client: ClientOperationPolicy, overrides: Partial<DiscoveredRoute> = {}): DiscoveredRoute {
  return {
    method: 'GET',
    path: '/runs/[run]/logs',
    streamMode: 'server',
    schemas: {
      params: { run: String },
      query: { cursor: String, limit: Number },
      returns: { cursor: String, line: String, count: Number, note: Optional(String) },
    },
    meta: { client },
    ...overrides,
  } as DiscoveredRoute;
}

function publish(route: DiscoveredRoute, options: OpenApiOptions = OPTIONS) {
  const operation = generateOpenApiSpec([route], options).paths['/runs/{run}/logs'].get['x-putnami-client'];
  if (!operation) throw new Error('the route published no client contract');
  return operation;
}

function refusal(route: DiscoveredRoute, options: OpenApiOptions = OPTIONS): string {
  try {
    generateOpenApiSpec([route], options);
  } catch (error) {
    return (error as Error).message;
  }
  return 'published';
}

describe('a declared sse continuation', () => {
  specTest(
    'is published once, on the sse transport',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-sse-continuation',
      check: 'a-declared-sse-continuation-is-published-on-the-sse-transport',
    },
    () => {
      for (const continuation of [CURSOR, { mode: 'best-effort' } as const]) {
        const operation = publish(
          logRoute({ sseContinuation: continuation, resilience: { stream: { reconnect: true } } }),
        );
        const sse = operation.transports.filter((transport) => transport.sse !== undefined);
        expect(sse).toHaveLength(1);
        expect(sse[0]?.protocol).toBe('sse');
        expect(sse[0]?.sse).toEqual({ continuation });
      }
      const plain = publish(logRoute({}));
      expect(plain.transports.some((transport) => 'sse' in transport)).toBe(false);
    },
  );

  specTest(
    'is refused on every shape that cannot carry it',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-sse-continuation',
      check: 'an-sse-continuation-is-refused-on-every-shape-that-cannot-carry-it',
    },
    () => {
      const best = { sseContinuation: { mode: 'best-effort' } } as const;
      const cases: [DiscoveredRoute, string][] = [
        [logRoute(best, { streamMode: undefined }), 'only a server stream can be continued'],
        [logRoute({ ...best, idempotency: { kind: 'non-idempotent' } }), 'only a safe stream can be continued'],
        [logRoute({ ...best, transports: ['websocket'] }), 'no sse transport is published to carry it'],
        [logRoute({ sseContinuation: { mode: 'replay' } as never }), 'is unsupported'],
        [
          logRoute({ sseContinuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: ' ' } } }),
          'does not name both its output field and its query parameter',
        ],
        [logRoute({ sseContinuation: { ...CURSOR, mode: 'best-effort' } as never }), 'carries no position'],
        [
          logRoute({ resilience: { stream: { reconnect: true } }, transports: ['sse'] }),
          'stream reconnect requires provider continuation support',
        ],
      ];
      for (const [route, want] of cases) {
        const message = refusal(route);
        expect(message).toContain('GET /runs/{run}/logs');
        expect(message).toContain(want);
      }
    },
  );

  specTest(
    'names a required plain-string output field and a declared plain-string query parameter',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-sse-continuation',
      check: 'a-cursor-names-a-required-plain-string-output-field-and-a-declared-query-parameter',
    },
    () => {
      const cases: [string, string, string][] = [
        ['position', 'cursor', 'names no property of the declared output message'],
        ['note', 'cursor', "output field 'note' must be required"],
        ['count', 'cursor', "output field 'count' must be a plain string"],
        ['cursor', 'after', "query parameter 'after' is not declared"],
        ['cursor', 'limit', "query parameter 'limit' must be a plain string"],
      ];
      for (const [outputField, queryParameter, want] of cases) {
        const message = refusal(
          logRoute({ sseContinuation: { mode: 'cursor', cursor: { outputField, queryParameter } } }),
        );
        expect(message).toContain('GET /runs/{run}/logs');
        expect(message).toContain(want);
      }
    },
  );

  it('keeps every framework-owned header out of a credential profile, as the Go reader does', () => {
    for (const header of [
      'X-Putnami-Stream-Wire',
      'Grpc-Timeout',
      'Connect-Timeout-Ms',
      'X-Request-Id',
      'X-Putnami-Client-Id',
    ]) {
      expect(
        refusal(logRoute({}), {
          ...OPTIONS,
          client: {
            service: { id: 'logs', audience: 'api://logs' },
            credentials: { key: { kind: 'api-key', header } },
          },
        }),
      ).toContain('framework-owned');
    }
  });

  it('resolves reconnect operation first, then from the document default for a server stream only', () => {
    const withDefault: OpenApiOptions = {
      ...OPTIONS,
      client: {
        service: { id: 'logs', audience: 'api://logs' },
        credentials: {},
        defaults: { resilience: { stream: { reconnect: true } } },
      },
    };
    expect(refusal(logRoute({ transports: ['sse'] }), withDefault)).toContain(
      'stream reconnect requires provider continuation support',
    );
    expect(refusal(logRoute({ transports: ['sse'], resilience: { stream: { reconnect: false } } }), withDefault)).toBe(
      'published',
    );
    expect(refusal(logRoute({ transports: ['sse'], sseContinuation: { mode: 'best-effort' } }), withDefault)).toBe(
      'published',
    );
    expect(
      refusal(
        logRoute({}, { streamMode: undefined, schemas: { query: { cursor: String }, returns: { line: String } } }),
        withDefault,
      ),
    ).toBe('published');
  });

  it('stands apart from external', () => {
    expect(refusal(logRoute({ external: 'npm registry API', sseContinuation: { mode: 'best-effort' } }))).toContain(
      'sseContinuation',
    );
  });
});
