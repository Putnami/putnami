import { type Application, application, http } from '@putnami/application';
import { ClientCredentialError } from '@putnami/client';
import { resetConfigLoader } from '@putnami/runtime';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  ItemsClient,
  registerItemsClient,
  registerTenantCheckClient,
  registerWhoamiClient,
  TenantCheckClient,
  WhoamiClient,
} from '../clients/ts/src';
import { SAMPLE_TENANT, USER_SUBJECT, USER_TOKEN } from '../src/caller-identity';
import { app as createApp } from '../src/main';
import { CATALOG_API_KEY } from '../src/workload-identity';

// TS→TS cell of the cross-language auth matrix: the five identity families a consumer
// and a provider of the same language exercise against each other. Every
// credential comes from the binding; no test writes a header on a request.

/** Every credential profile the provider declares, all bound. */
const ALL_CREDENTIALS = {
  'catalog-key': { source: 'static', value: CATALOG_API_KEY },
  tenant: { source: 'static', value: SAMPLE_TENANT },
  // Forwarding the caller's own token is opt-in per binding; the value comes
  // per call from the inbound request, never from configuration.
  user: { source: 'forwarded-user' },
} as const;

/** The feature this sample owns in putnami.features.json. */
const FEATURE = 'samples/ts-first-party-client-matrix';

describe('service-to-service identity', () => {
  let provider: Application;
  let providerUrl: string;
  const originalConfig = process.env.CONFIG_DATA;

  beforeAll(async () => {
    const reservation = Bun.serve({ port: 0, fetch: () => new Response(null, { status: 503 }) });
    const port = reservation.port;
    reservation.stop(true);
    providerUrl = `http://localhost:${port}`;
    bindTo(providerUrl, ALL_CREDENTIALS);
    provider = createApp({ port });
    await provider.start();
  });

  afterAll(async () => {
    await provider.stop();
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  it('carries both credentials of one alternative together', async () => {
    const consumer = await boundConsumer(providerUrl, ALL_CREDENTIALS);
    try {
      const check = await consumer.app.context.get(TenantCheckClient).getTenant_check();
      expect(check).toEqual({ key: true, tenant: true });
    } finally {
      await consumer.stop();
    }
  });

  specTest(
    'refuses a half-satisfied alternative before the request leaves the consumer',
    {
      feature: FEATURE,
      requirement: 'identity-is-declared-and-never-downgraded',
      check: 'an-alternative-is-satisfied-whole-or-not-at-all',
    },
    async () => {
      const recorder = recordingProvider();
      const consumer = await boundConsumer(recorder.url, {
        'catalog-key': { source: 'static', value: CATALOG_API_KEY },
      });
      try {
        const call = consumer.app.context.get(TenantCheckClient).getTenant_check();
        // The refusal is the consumer's own, not a status the provider returned.
        await expect(call).rejects.toBeInstanceOf(ClientCredentialError);
        expect(recorder.requests.length).toBe(0);
      } finally {
        await consumer.stop();
        recorder.stop();
      }
    },
  );

  specTest(
    'names the user the consumer forwarded from its own inbound request',
    {
      feature: FEATURE,
      requirement: 'identity-is-declared-and-never-downgraded',
      check: 'a-forwarded-user-identity-comes-from-the-inbound-request',
    },
    async () => {
      const consumer = await consumerBehindItsOwnRoute(providerUrl, ALL_CREDENTIALS);
      try {
        const response = await fetch(`${consumer.url}/me`, {
          headers: { Authorization: `Bearer ${USER_TOKEN}` },
        });
        expect(response.status).toBe(200);
        // The provider names the user from the identity its resolver built, and
        // the token itself never appears in the answer.
        expect(await response.json()).toEqual({ subject: USER_SUBJECT });
      } finally {
        await consumer.stop();
      }
    },
  );

  it('never calls anonymously when the inbound request carries no identity', async () => {
    const recorder = recordingProvider();
    const consumer = await consumerBehindItsOwnRoute(recorder.url, ALL_CREDENTIALS);
    try {
      const response = await fetch(`${consumer.url}/me`);
      expect(response.status).not.toBe(200);
      expect(recorder.requests.length).toBe(0);
    } finally {
      await consumer.stop();
      recorder.stop();
    }
  });

  it('forwards nothing when the binding never opted in', async () => {
    const recorder = recordingProvider();
    const consumer = await consumerBehindItsOwnRoute(recorder.url, {
      'catalog-key': { source: 'static', value: CATALOG_API_KEY },
      tenant: { source: 'static', value: SAMPLE_TENANT },
    });
    try {
      const response = await fetch(`${consumer.url}/me`, {
        headers: { Authorization: `Bearer ${USER_TOKEN}` },
      });
      expect(response.status).not.toBe(200);
      expect(recorder.requests.length).toBe(0);
    } finally {
      await consumer.stop();
      recorder.stop();
    }
  });

  specTest(
    'sends a forwarded token only to the operation that declares it',
    {
      feature: FEATURE,
      requirement: 'identity-is-declared-and-never-downgraded',
      check: 'a-credential-reaches-only-the-operation-that-declares-it',
    },
    async () => {
      const recorder = recordingProvider();
      const consumer = await consumerBehindItsOwnRoute(recorder.url, ALL_CREDENTIALS);
      try {
        // The same inbound request lists items, an operation that declares no
        // user credential and no api key.
        const response = await fetch(`${consumer.url}/list`, {
          headers: { Authorization: `Bearer ${USER_TOKEN}` },
        });
        expect(response.status).toBe(200);
        expect(recorder.requests.length).toBe(1);
        expect(recorder.requests[0]?.get('authorization')).toBeNull();
        expect(recorder.requests[0]?.get('x-catalog-key')).toBeNull();
      } finally {
        await consumer.stop();
        recorder.stop();
      }
    },
  );
});

type Credentials = Readonly<Record<string, { readonly source: string; readonly value?: string }>>;

/** Point the process configuration at `url` with exactly `credentials`. */
function bindTo(url: string, credentials: Credentials): void {
  process.env.CONFIG_DATA = JSON.stringify({
    clients: {
      clientId: 'service-to-service-sample',
      services: { 'catalog.items': { url, allowInsecure: true, credentials } },
    },
  });
  resetConfigLoader();
}

/** A consumer application holding the generated clients, bound to `url`. */
async function boundConsumer(
  url: string,
  credentials: Credentials,
): Promise<{ app: Application; stop: () => Promise<void> }> {
  bindTo(url, credentials);
  const app = application();
  registerItemsClient(app);
  registerTenantCheckClient(app);
  registerWhoamiClient(app);
  await app.start();
  return { app, stop: () => app.stop() };
}

/**
 * A consumer serving its own routes, which is what makes a forwarded identity
 * real: the token is the one its inbound request carried, and the runtime
 * carries it from that request's context to the outbound call.
 */
async function consumerBehindItsOwnRoute(
  url: string,
  credentials: Credentials,
): Promise<{ url: string; stop: () => Promise<void> }> {
  bindTo(url, credentials);
  let app: Application;
  const server = http({ port: 0 })
    // The route passes no token and writes no header: it calls the generated
    // client, and the runtime supplies the identity of the request in flight.
    .get('/me', () => app.context.get(WhoamiClient).getWhoami())
    .get('/list', () =>
      app.context.get(ItemsClient).getItems({
        query: { search: '', limit: 10 },
        headers: { 'x-catalog-tenant': 'tenant-a' },
      }),
    );
  app = application().use(server);
  registerItemsClient(app);
  registerTenantCheckClient(app);
  registerWhoamiClient(app);
  await app.start();
  return { url: `http://localhost:${server.getServer()?.port}`, stop: () => app.stop() };
}

/**
 * A provider that records the requests it receives. It is how a test asserts
 * what did *not* leave the consumer: an absent request and an absent header are
 * both observable here.
 */
function recordingProvider(): { url: string; requests: Headers[]; stop: () => void } {
  const requests: Headers[] = [];
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      requests.push(request.headers);
      // The shape the items contract declares, so a call that does reach this
      // provider fails on its headers and never on its reply.
      return Response.json({ items: [], tenant: 'tenant-a' });
    },
  });
  return { url: `http://localhost:${server.port}`, requests, stop: () => server.stop(true) };
}
