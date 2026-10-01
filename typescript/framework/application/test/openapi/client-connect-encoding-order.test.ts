import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { ClientOperationPolicy } from '../../src/api/client-contract';
import type { DiscoveredRoute } from '../../src/api/discovered-route.type';
import { generateOpenApiSpec, type OpenApiOptions } from '../../src/openapi/openapi';

// The gRPC plugin serves Connect in protobuf then JSON, and a generated client
// dispatches the first declared entry it can carry, so without a declaration
// the second encoding never travels. These tests read the published contract
// the way every emitter does.

const CONNECT: NonNullable<OpenApiOptions['connect']> = { packageName: 'example.quotes.v1' };
const OPTIONS: OpenApiOptions = {
  info: { title: 'Quotes', version: '1.0.0' },
  client: {
    service: { id: 'quotes', audience: 'api://quotes' },
    credentials: {},
  },
  connect: CONNECT,
};

const METHOD = '/example.quotes.v1.QuotesService/GetQuotesById';

function quoteRoute(client: ClientOperationPolicy): DiscoveredRoute {
  return {
    method: 'GET',
    path: '/quotes/[id]',
    schemas: { params: { id: String }, returns: { id: String } },
    meta: { client },
  };
}

function publishedWires(client: ClientOperationPolicy, connect: OpenApiOptions['connect'] = CONNECT): string[] {
  const spec = generateOpenApiSpec([quoteRoute(client)], { ...OPTIONS, connect });
  const transports = spec.paths['/quotes/{id}'].get['x-putnami-client']?.transports ?? [];
  for (const transport of transports) {
    if (transport.protocol === 'connect') {
      expect(transport).toMatchObject({ path: METHOD, protobufMethod: METHOD });
    }
  }
  return transports.map((transport) => `${transport.protocol}/${transport.encoding}`);
}

describe('a declared Connect encoding order', () => {
  specTest(
    'is the order the published contract carries',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-connect-encoding-order',
      check: 'a-declared-connect-encoding-order-is-the-published-order',
    },
    () => {
      // The plugin order when the operation declares none.
      expect(publishedWires({})).toEqual(['connect/proto', 'connect/json', 'rest-json/json']);
      // JSON first keeps the REST entry where the framework put it.
      expect(publishedWires({ connectEncodings: ['json', 'proto'] })).toEqual([
        'connect/json',
        'connect/proto',
        'rest-json/json',
      ]);
      // A Connect-only operation declaring JSON first dispatches JSON.
      expect(publishedWires({ transports: ['connect'], connectEncodings: ['json', 'proto'] })).toEqual([
        'connect/json',
        'connect/proto',
      ]);
      // A narrowed order publishes only the encoding it names, after the
      // transport the operation put in front.
      expect(publishedWires({ transports: ['rest-json', 'connect'], connectEncodings: ['json'] })).toEqual([
        'rest-json/json',
        'connect/json',
      ]);
    },
  );

  specTest(
    'cannot publish an encoding the gRPC plugin does not serve',
    {
      feature: 'typescript/api-contracts',
      requirement: 'declared-connect-encoding-order',
      check: 'a-connect-encoding-order-cannot-publish-an-encoding-the-plugin-does-not-serve',
    },
    () => {
      const route = 'api.client operation GET /quotes/{id}';
      // The plugin accepts only JSON on this route, so protobuf is not served.
      expect(() => publishedWires({ connectEncodings: ['proto'] }, { ...CONNECT, unaryEncodings: ['json'] })).toThrow(
        `${route}: Connect encoding 'proto' is declared but the gRPC plugin does not serve it for this route`,
      );
      expect(() => publishedWires({ connectEncodings: ['json', 'json'] })).toThrow(
        `${route}: Connect encoding 'json' is declared twice in the client encoding order`,
      );
      // No gRPC plugin mounted, and an operation narrowed away from Connect,
      // both publish no Connect transport for the order to apply to.
      expect(() =>
        generateOpenApiSpec([quoteRoute({ connectEncodings: ['json'] })], { ...OPTIONS, connect: undefined }),
      ).toThrow(`${route}: a Connect encoding order is declared and no Connect transport is published`);
      expect(() => publishedWires({ transports: ['rest-json'], connectEncodings: ['json'] })).toThrow(
        `${route}: a Connect encoding order is declared and no Connect transport is published`,
      );
    },
  );
});
