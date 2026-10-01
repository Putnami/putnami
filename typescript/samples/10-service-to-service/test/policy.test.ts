import { type Application, application } from '@putnami/application';
import { resetConfigLoader } from '@putnami/runtime';
import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { ItemsClient, registerItemsClient } from '../clients/ts/src';
import { IDEMPOTENCY_KEY_HEADER } from '../src/api/items/post';
import { CATALOG_API_KEY } from '../src/workload-identity';

// Policy cell of the cross-language interop matrix: the request policy the create operation
// declares — an identity header, bounded attempts, a per-attempt and a total
// budget, and a circuit — decided by the contract and applied by the generated
// client. Nothing here writes a retry loop, a header or a deadline.

/**
 * A provider that answers `/items` with the statuses `plan` lists, one per
 * attempt, and records what each attempt carried. A plan shorter than the
 * attempts made repeats its last entry.
 */
function policyProvider(plan: number[], delayMs = 0) {
  const received: Headers[] = [];
  const server = Bun.serve({
    port: 0,
    async fetch(request) {
      const attempt = received.length;
      received.push(request.headers);
      if (delayMs > 0) await Bun.sleep(delayMs);
      const status = attempt < plan.length ? (plan[attempt] as number) : (plan[plan.length - 1] as number);
      if (status < 300) {
        return Response.json({ item: { id: '3', name: 'Sprocket', price: 1, stock: 1 } }, { status });
      }
      return Response.json({ error: 'try again' }, { status });
    },
  });
  return { url: `http://localhost:${server.port}`, received, stop: () => server.stop(true) };
}

/** The feature this sample owns in putnami.features.json. */
const FEATURE = 'samples/ts-first-party-client-matrix';

describe('service-to-service request policy', () => {
  let consumer: Application | undefined;
  const originalConfig = process.env.CONFIG_DATA;

  afterEach(async () => {
    await consumer?.stop();
    consumer = undefined;
    if (originalConfig === undefined) delete process.env.CONFIG_DATA;
    else process.env.CONFIG_DATA = originalConfig;
    resetConfigLoader();
  });

  /** A consumer holding the generated items client, bound to `url`. */
  async function boundTo(url: string): Promise<ItemsClient> {
    process.env.CONFIG_DATA = JSON.stringify({
      clients: {
        clientId: 'service-to-service-sample',
        services: {
          'catalog.items': {
            url,
            allowInsecure: true,
            credentials: { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } },
          },
        },
      },
    });
    resetConfigLoader();
    consumer = application();
    registerItemsClient(consumer);
    await consumer.start();
    return consumer.context.get(ItemsClient);
  }

  specTest(
    'repeats the same request identity across the declared attempts',
    {
      feature: FEATURE,
      requirement: 'the-declared-request-policy-is-the-one-applied',
      check: 'a-declared-retry-repeats-one-request-identity',
    },
    async () => {
      const provider = policyProvider([503, 503, 200]);
      try {
        const created = await (await boundTo(provider.url)).postItems({
          body: { name: 'Sprocket', price: 1, stock: 1 },
        });
        expect(created.item.id).toBe('3');

        expect(provider.received).toHaveLength(3);
        const key = provider.received[0]?.get(IDEMPOTENCY_KEY_HEADER);
        expect(key).toBeTruthy();
        // One identity for the call, not one per attempt: that is what lets the
        // provider recognize a repeat instead of creating a second item.
        for (const headers of provider.received) {
          expect(headers.get(IDEMPOTENCY_KEY_HEADER)).toBe(key ?? '');
        }
      } finally {
        provider.stop();
      }
    },
  );

  specTest(
    'treats the declared attempt count as a bound',
    {
      feature: FEATURE,
      requirement: 'the-declared-request-policy-is-the-one-applied',
      check: 'declared-attempts-are-a-bound',
    },
    async () => {
      const provider = policyProvider([503]);
      try {
        const client = await boundTo(provider.url);
        await expect(client.postItems({ body: { name: 'Sprocket', price: 1, stock: 1 } })).rejects.toMatchObject({
          status: 503,
        });
        expect(provider.received).toHaveLength(3);
      } finally {
        provider.stop();
      }
    },
  );

  specTest(
    'ends an attempt that never answers on the declared budget',
    {
      feature: FEATURE,
      requirement: 'the-declared-request-policy-is-the-one-applied',
      check: 'a-declared-budget-ends-an-attempt-that-never-answers',
    },
    async () => {
      // Each attempt is bounded at 250ms and the whole call at 2s, so a provider
      // that takes a second per attempt can only end one way.
      const provider = policyProvider([200], 1000);
      try {
        const client = await boundTo(provider.url);
        const started = performance.now();
        await expect(client.postItems({ body: { name: 'Sprocket', price: 1, stock: 1 } })).rejects.toBeDefined();
        expect(performance.now() - started).toBeLessThan(2500);
        expect(provider.received.length).toBeGreaterThan(0);
      } finally {
        provider.stop();
      }
    },
  );

  specTest(
    'stops calling once the declared failure threshold opens the circuit',
    {
      feature: FEATURE,
      requirement: 'the-declared-request-policy-is-the-one-applied',
      check: 'a-declared-circuit-stops-calling',
    },
    async () => {
      const provider = policyProvider([503]);
      try {
        const client = await boundTo(provider.url);
        const body = { body: { name: 'Sprocket', price: 1, stock: 1 } };
        // The circuit counts calls, not attempts: two failed calls reach the
        // declared threshold.
        await expect(client.postItems(body)).rejects.toBeDefined();
        await expect(client.postItems(body)).rejects.toBeDefined();
        const before = provider.received.length;

        await expect(client.postItems(body)).rejects.toMatchObject({ code: 'client.circuit_open' });
        // Nothing left the consumer: an open circuit is a refusal, not a call.
        expect(provider.received).toHaveLength(before);
      } finally {
        provider.stop();
      }
    },
  );
});
