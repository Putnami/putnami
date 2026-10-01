import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { api, application, endpoint, http, type HttpMiddleware, type SecurityOptions } from '@putnami/application';
import { generateOpenApiSpec } from '../../src/openapi/openapi';
import { SecurityMiddleware } from '../../src/security/security.middleware';
import type { HttpRequestContext } from '../../src/http/http-context.type';

/**
 * An optional credential is an endpoint that answers anonymous and authenticated
 * callers alike — a public package read that answers more to an authorized
 * caller. `.secure({ optional: true })` says so; the contract then offers the
 * credential first and the anonymous alternative last. Mirrors
 * go/framework/openapi/optional_credential_test.go (ADR 0002 of
 * go/framework/security).
 */

const FEATURE = 'typescript/api-contracts';
const REQUIREMENT = 'optional-credential';

const client = {
  service: { id: 'put-server', audience: 'put-server' },
  credentials: { user: { kind: 'forwarded-user-token' as const } },
};
const user = { allOf: [{ profile: 'user', scopes: ['packages:read'] }] };
const anonymous = { allOf: [] };

function publish(security: SecurityOptions, alternatives: unknown[]) {
  return generateOpenApiSpec(
    [
      {
        method: 'GET',
        path: '/packages/[name]/resolve',
        schemas: { params: { name: String }, returns: { caller: String } },
        meta: {
          security,
          client: { security: { alternatives: alternatives as never }, idempotency: { kind: 'safe' } },
        },
      },
    ],
    { info: { title: 'Packages', version: '1.0.0' }, client },
  );
}

describe('optional credential', () => {
  specTest(
    'publishes the credential then the anonymous alternative for an optional endpoint',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'an-optional-endpoint-publishes-its-credential-then-the-anonymous-alternative',
    },
    () => {
      const operation = publish({ optional: true, scopes: ['packages:read'] }, [user, anonymous]).paths[
        '/packages/{name}/resolve'
      ].get;
      expect(operation['x-putnami-client'].security).toEqual({
        alternatives: [user, anonymous],
        authorization: { scopesAll: ['packages:read'] },
      });
      // The plain OpenAPI document says the same thing in its own vocabulary:
      // the empty security requirement is the anonymous alternative.
      expect(operation.security).toEqual([{ bearerAuth: [] }, {}]);
    },
  );

  specTest(
    'refuses an anonymous alternative unless the endpoint is optional and it comes last',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'an-anonymous-alternative-is-refused-unless-the-endpoint-is-optional-and-it-comes-last',
    },
    () => {
      expect(() => publish({ scopes: ['packages:read'] }, [user, anonymous])).toThrow(
        'an authenticated endpoint cannot advertise an anonymous client alternative',
      );
      expect(() => publish({ optional: true, scopes: ['packages:read'] }, [anonymous, user])).toThrow(
        'the anonymous client alternative must come last',
      );
      // Optional does not relax what a credentialed alternative must request.
      expect(() =>
        publish({ optional: true, scopes: ['packages:read'] }, [{ allOf: [{ profile: 'user' }] }, anonymous]),
      ).toThrow("client security alternative does not request required scope 'packages:read'");
    },
  );

  specTest(
    'serves an anonymous caller, authorizes an authenticated one, and refuses an unresolved credential',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'an-optional-endpoint-serves-an-anonymous-caller-and-refuses-an-unresolved-credential',
    },
    async () => {
      const resolver: HttpMiddleware = (context, next) => {
        const header = context.req.headers.get('Authorization');
        if (header === 'Bearer reader-token') context.user = { sub: 'reader', scope: 'packages:read' };
        if (header === 'Bearer stranger-token') context.user = { sub: 'stranger' };
        return next();
      };
      const providerHttp = http({ port: 0 });
      providerHttp.prepend(resolver);
      const providerApi = api({ autoScan: false, client });
      providerApi.register(
        '/packages/[name]/resolve',
        endpoint()
          .params({ name: String })
          .returns({ caller: String })
          .secure({ optional: true, scopes: ['packages:read'] })
          .client({ security: { alternatives: [user, anonymous] }, idempotency: { kind: 'safe' } })
          .handle((context) => ({ caller: typeof context.user?.sub === 'string' ? context.user.sub : 'anonymous' })),
        'GET',
      );
      const app = application().use(providerHttp).use(providerApi);
      await app.start();
      try {
        const port = providerHttp.getServer()?.port;
        const call = (authorization?: string) =>
          fetch(`http://localhost:${port}/packages/private/resolve`, {
            headers: authorization ? { Authorization: authorization } : {},
          });
        const anonymousCall = await call();
        expect(anonymousCall.status).toBe(200);
        expect(await anonymousCall.json()).toEqual({ caller: 'anonymous' });
        const readerCall = await call('Bearer reader-token');
        expect(readerCall.status).toBe(200);
        expect(await readerCall.json()).toEqual({ caller: 'reader' });
        // Presenting a credential is never a downgrade: it is authorized like a
        // required rule would, and refused when no resolver accepts it.
        expect((await call('Bearer stranger-token')).status).toBe(403);
        expect((await call('Bearer expired-token')).status).toBe(401);
      } finally {
        await app.stop();
      }

      // The rule itself, without a server: `authorization` undefined sends no
      // header, and `seen` is the identity the handler would read.
      const run = async (options: SecurityOptions, authorization?: string, preset?: Record<string, unknown>) => {
        let seen: unknown = 'not served';
        const context = {
          req: new Request('http://localhost/packages', {
            headers: authorization === undefined ? {} : { Authorization: authorization },
          }),
          method: 'GET',
          path: () => '/packages',
          user: preset,
        } as HttpRequestContext;
        const response = await SecurityMiddleware(options)(context, async () => {
          seen = context.user;
          return undefined as never;
        });
        return { status: seen === 'not served' ? (response as { status: number }).status : 200, seen };
      };

      // An empty or blank Authorization header presents no credential: it is
      // absent for the rule, as it is in the Go twin.
      expect(await run({ optional: true }, '')).toEqual({ status: 200, seen: undefined });
      expect(await run({ optional: true }, '   ')).toEqual({ status: 200, seen: undefined });
      expect((await run({ optional: true }, 'Bearer unresolved')).status).toBe(401);

      // A custom verifier follows the same rule: no credential is anonymous, a
      // refused or unreadable one is 401.
      const verify = (token: string) => (token === 'good' ? { sub: 'v' } : undefined);
      expect((await run({ optional: true, verify })).status).toBe(200);
      expect((await run({ optional: true, verify }, '')).status).toBe(200);
      expect((await run({ optional: true, verify }, 'Bearer good')).seen).toMatchObject({ sub: 'v' });
      expect((await run({ optional: true, verify }, 'Bearer bad')).status).toBe(401);
      expect((await run({ optional: true, verify }, 'Basic dXNlcjpwYXNz')).status).toBe(401);

      // The verifier is the route's only identity source. An identity another
      // resolver already set (here an API-key caller holding the role) was never
      // verified by it: without a bearer token the handler sees an anonymous
      // caller, and a required verifier refuses the request.
      const apiKeyCaller = { sub: 'api-key-caller', kind: 'apikey', roles: ['admin'] };
      expect(await run({ optional: true, verify, roles: ['admin'] }, undefined, apiKeyCaller)).toEqual({
        status: 200,
        seen: undefined,
      });
      expect(await run({ verify, roles: ['admin'] }, undefined, apiKeyCaller)).toEqual({
        status: 401,
        seen: 'not served',
      });
    },
  );
});
