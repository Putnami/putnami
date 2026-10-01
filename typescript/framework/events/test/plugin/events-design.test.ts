import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';
import {
  buildDesignGraph,
  type DesignEdge,
  type DesignGraph,
  registerModuleLoader,
  serializeDesignGraph,
} from '@putnami/application';
import { resetConfigLoader, Uuid } from '@putnami/runtime';
import { EventsPlugin } from '../../src/events.plugin';
import { handler } from '../../src/handler/handler';
import { outbox } from '../../src/outbox/outbox';
import { clearDesignPublications } from '../../src/publisher/publisher';
import { topic } from '../../src/topic/topic';
import type { Envelope, Transport } from '../../src/transport';

const eventsRoot = resolve(import.meta.dir, '..', '..');
const applicationEntry = resolve(eventsRoot, '..', 'application', 'src', 'index.ts');
const eventsEntry = join(eventsRoot, 'src', 'index.ts');

const SECRET_METADATA_VALUE = 'sk-live-never-in-a-design-artifact';
const SECRET_DEFAULT_VALUE = 'super-secret-signing-default';

/**
 * Every file under `typescript/framework/<project>/` is framework-internal to
 * the stack walker, so a declaration written inside this test file would carry
 * no `__source` and would mint no feature scope. The regression fixture
 * therefore lives in a throwaway project root outside the framework tree and
 * imports the runtime through relative paths, which resolve to the same modules
 * as the package specifiers used here.
 */
function createFixtureRoot(prefix: string, cleanupPaths: Set<string>): string {
  // macOS resolves the temp directory through /private. getExternalCaller
  // compares the resolved stack path against the project root, so an
  // unresolved root would relativize to '../..' and drop every provenance.
  const root = realpathSync(mkdtempSync(join(tmpdir(), prefix)));
  cleanupPaths.add(root);
  process.env.PUTNAMI_PROJECT_ROOT = root;
  return root;
}

function writeFixtureModule(root: string, file: string, lines: (specifiers: FixtureSpecifiers) => string[]): string {
  const path = join(root, file);
  const specifiers: FixtureSpecifiers = {
    application: importSpecifier(path, applicationEntry),
    events: importSpecifier(path, eventsEntry),
  };
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, `${lines(specifiers).join('\n')}\n`);
  return path;
}

interface FixtureSpecifiers {
  application: string;
  events: string;
}

function importSpecifier(from: string, target: string): string {
  const specifier = relative(dirname(from), target).replaceAll('\\', '/');
  return specifier.startsWith('.') ? specifier : `./${specifier}`;
}

function edgeOf(graph: DesignGraph | undefined, from: string, to: string, kind: string): DesignEdge | undefined {
  return graph?.edges.find((candidate) => candidate.from === from && candidate.to === to && candidate.kind === kind);
}

async function importFixtureApplication(path: string): Promise<Parameters<typeof buildDesignGraph>[0]> {
  const fixture = (await import(path)) as { app: Parameters<typeof buildDesignGraph>[0] };
  return fixture.app;
}

describe('native events design contributions', () => {
  const cleanupPaths = new Set<string>();

  beforeEach(() => {
    registerModuleLoader(
      'events-loader',
      mock(async () => undefined),
    );
    process.env.PUTNAMI_PROJECT_ROOT = eventsRoot;
    delete process.env.EVENTS_ENDPOINT;
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
    // The publisher call-site registry is a process global. Clear it so the
    // fixture's own top-level getPublisher() calls are the only recorded sites.
    clearDesignPublications();
  });

  afterEach(() => {
    for (const path of cleanupPaths) {
      rmSync(path, { recursive: true, force: true });
    }
    cleanupPaths.clear();
    delete process.env.PUTNAMI_PROJECT_ROOT;
    delete process.env.EVENTS_ENDPOINT;
    delete process.env.CONFIG_DATA;
    resetConfigLoader();
    clearDesignPublications();
  });

  it('attributes root plugin infrastructure through explicit native declaration sources', async () => {
    const root = createFixtureRoot('putnami-events-root-design-', cleanupPaths);
    const modulePath = writeFixtureModule(root, 'src/identity/identity.module.ts', (specifier) => [
      `import { module } from '${specifier.application}';`,
      `import { Uuid, events, handler, topic } from '${specifier.events}';`,
      '',
      `export const SessionRevoked = topic('identity.session.revoked', { sessionId: Uuid });`,
      `const consume = handler(SessionRevoked).handle(async () => {});`,
      '',
      `export const app = module('app')`,
      `  .use(events({ autoScan: false, handlers: [consume] }))`,
      `  .use(module('identity').feature({`,
      `    id: 'identity/opaque-tokens',`,
      `    name: 'Opaque token revocation',`,
      `    outcome: 'Operators can revoke an issued opaque token',`,
      `    owner: 'identity',`,
      `  }, { sources: ['src/identity'] }));`,
    ]);

    const graph = await buildDesignGraph(await importFixtureApplication(modulePath), '@example/identity', root);
    const infraNode = graph?.nodes.find(({ id }) => id === 'infra:events:identity.session.revoked');
    const expectedInfraNode = {
      id: 'infra:events:identity.session.revoked',
      kind: 'infra',
      name: 'identity.session.revoked',
      properties: { kind: 'events' },
    };
    if (JSON.stringify(infraNode) !== JSON.stringify(expectedInfraNode)) {
      throw new Error(`root events infra node = ${JSON.stringify(infraNode)}`);
    }
    const infraEdge = edgeOf(
      graph,
      'module:@example/identity/identity',
      'infra:events:identity.session.revoked',
      'contains',
    );
    const expectedInfraEdge = {
      from: 'module:@example/identity/identity',
      to: 'infra:events:identity.session.revoked',
      kind: 'contains',
      authority: 'derived',
      provenance: { path: 'src/identity/identity.module.ts' },
    };
    if (JSON.stringify(infraEdge) !== JSON.stringify(expectedInfraEdge)) {
      throw new Error(`root events infra edge = ${JSON.stringify(infraEdge)}`);
    }
  });

  it('models enqueue, relay publication, transport publication, and subscription as distinct relationships', async () => {
    const root = createFixtureRoot('putnami-events-design-', cleanupPaths);
    const modulePath = writeFixtureModule(root, 'src/identity/identity.module.ts', (specifier) => [
      `import { module } from '${specifier.application}';`,
      `import { Default, Uuid, events, getPublisher, handler, outbox, topic } from '${specifier.events}';`,
      '',
      `export const SessionRevoked = topic('identity.session.revoked', { sessionId: Uuid, tenantId: Uuid }, {`,
      `  metadata: { signingKey: '${SECRET_METADATA_VALUE}' },`,
      '});',
      `export const TokenRevoked = topic('identity.token.revoked', { tokenId: Uuid });`,
      `export const TokenIssued = topic('identity.token.issued', {`,
      '  tokenId: Uuid,',
      `  secret: Default(String, '${SECRET_DEFAULT_VALUE}'),`,
      '});',
      '',
      `export const IdentityOutbox = outbox('identity', {`,
      '  topics: [SessionRevoked, TokenRevoked],',
      "  table: 'auth.event_outbox',",
      "  datasource: 'identity',",
      '});',
      '',
      'export const publishTokenIssued = getPublisher(TokenIssued);',
      '',
      `export const app = module('app').use(`,
      `  module('identity')`,
      '    .feature({',
      "      id: 'identity/opaque-tokens',",
      "      name: 'Opaque token revocation',",
      "      outcome: 'Operators can revoke an issued opaque token',",
      "      owner: 'identity',",
      '    })',
      '    .use(',
      '      events({',
      '        autoScan: false,',
      '        outboxes: [IdentityOutbox],',
      '        handlers: [handler(SessionRevoked).handle(async () => {})],',
      '      }),',
      '    ),',
      ');',
    ]);

    const graph = await buildDesignGraph(await importFixtureApplication(modulePath), '@example/identity', root);
    const moduleId = 'module:@example/identity/identity';
    const outboxId = 'event.outbox:identity';

    // The outbox is its own surface, carrying bounded identifiers only.
    const outboxNode = graph?.nodes.find(({ id }) => id === outboxId);
    expect(outboxNode?.kind).toBe('event.outbox');
    expect(outboxNode?.name).toBe('identity');
    expect(outboxNode?.properties).toEqual({ datasource: 'identity', table: 'auth.event_outbox' });
    expect(outboxNode?.provenance?.path).toBe('src/identity/identity.module.ts');

    // A row committed to the outbox is not a transport publication: the
    // commit-time enqueue and the relay-time publication are separate exact
    // edges, and a direct publisher call site stays a derived module publish.
    expect(edgeOf(graph, moduleId, outboxId, 'enqueues')).toEqual({
      from: moduleId,
      to: outboxId,
      kind: 'enqueues',
      authority: 'exact',
    });
    expect(edgeOf(graph, outboxId, 'event.topic:identity.session.revoked', 'publishes')?.authority).toBe('exact');
    // A relay-only topic has no local publisher and no handler; the outbox
    // declaration is the sole reason its node exists.
    expect(graph?.nodes.some(({ id }) => id === 'event.topic:identity.token.revoked')).toBe(true);
    expect(edgeOf(graph, outboxId, 'event.topic:identity.token.revoked', 'publishes')?.authority).toBe('exact');
    expect(edgeOf(graph, moduleId, 'event.topic:identity.token.issued', 'publishes')?.authority).toBe('derived');
    expect(edgeOf(graph, moduleId, 'event.topic:identity.session.revoked', 'publishes')).toBeUndefined();
    expect(edgeOf(graph, moduleId, outboxId, 'publishes')).toBeUndefined();

    // Subscription keeps its own native authority, unchanged by the outbox.
    const handlerNode = graph?.nodes.find(({ kind }) => kind === 'event.handler');
    expect(handlerNode?.properties).toEqual({ topic: 'identity.session.revoked' });
    expect(edgeOf(graph, handlerNode?.id ?? '', 'event.topic:identity.session.revoked', 'subscribes')?.authority).toBe(
      'exact',
    );
  });

  it('forwards outbox contributions from a plugin that owns the events plugin privately', async () => {
    const root = createFixtureRoot('putnami-events-delegate-', cleanupPaths);
    const modulePath = writeFixtureModule(root, 'src/identity/relay.module.ts', (specifier) => [
      `import { module } from '${specifier.application}';`,
      `import { Uuid, events, outbox, topic } from '${specifier.events}';`,
      '',
      `export const SessionRevoked = topic('identity.session.revoked', { sessionId: Uuid });`,
      `export const IdentityOutbox = outbox('identity', { topics: [SessionRevoked], table: 'auth.event_outbox' });`,
      '',
      '// Stands in for a relay controller that composes the events plugin',
      '// privately instead of mounting it on the module tree.',
      'class RelayController {',
      '  readonly events = events({ autoScan: false, outboxes: [IdentityOutbox] });',
      '  designDelegates() {',
      '    return [this.events];',
      '  }',
      '}',
      '',
      `export const app = module('app').use(`,
      `  module('identity')`,
      '    .feature({',
      "      id: 'identity/opaque-tokens',",
      "      name: 'Opaque token revocation',",
      "      outcome: 'Operators can revoke an issued opaque token',",
      "      owner: 'identity',",
      '    })',
      '    .use(new RelayController()),',
      ');',
    ]);

    const graph = await buildDesignGraph(await importFixtureApplication(modulePath), '@example/identity', root);

    expect(graph?.nodes.some(({ id }) => id === 'event.outbox:identity')).toBe(true);
    expect(edgeOf(graph, 'module:@example/identity/identity', 'event.outbox:identity', 'enqueues')?.authority).toBe(
      'exact',
    );
    expect(edgeOf(graph, 'event.outbox:identity', 'event.topic:identity.session.revoked', 'publishes')?.authority).toBe(
      'exact',
    );
  });

  it('keeps secret payload values out of the serialized design artifact', async () => {
    const root = createFixtureRoot('putnami-events-secret-', cleanupPaths);
    const modulePath = writeFixtureModule(root, 'src/billing/billing.module.ts', (specifier) => [
      `import { module } from '${specifier.application}';`,
      `import { Default, Uuid, events, getPublisher, outbox, topic } from '${specifier.events}';`,
      '',
      `export const InvoicePaid = topic(`,
      `  'billing.invoice.paid',`,
      `  { invoiceId: Uuid, apiKey: Default(String, '${SECRET_DEFAULT_VALUE}') },`,
      `  { metadata: { webhookSecret: '${SECRET_METADATA_VALUE}' } },`,
      ');',
      '',
      `export const BillingOutbox = outbox('billing', { topics: [InvoicePaid], table: 'billing.event_outbox' });`,
      '',
      'export const publishInvoicePaid = getPublisher(InvoicePaid);',
      '',
      `export const app = module('app').use(`,
      `  module('billing')`,
      '    .feature({',
      "      id: 'billing/invoicing',",
      "      name: 'Invoicing',",
      "      outcome: 'Customers can pay an issued invoice',",
      "      owner: 'billing',",
      '    })',
      '    .use(events({ autoScan: false, outboxes: [BillingOutbox] })),',
      ');',
    ]);

    const graph = await buildDesignGraph(await importFixtureApplication(modulePath), '@example/billing', root);
    const serialized = serializeDesignGraph(graph as DesignGraph);

    // Field names are bounded identifiers and are useful design facts; the
    // values behind them are not, and topic metadata is never projected at all.
    expect(graph?.nodes.find(({ id }) => id === 'event.topic:billing.invoice.paid')?.properties).toEqual({
      fields: 'apiKey,invoiceId',
    });
    expect(serialized).not.toContain(SECRET_DEFAULT_VALUE);
    expect(serialized).not.toContain(SECRET_METADATA_VALUE);
    expect(serialized).not.toContain('webhookSecret');
  });

  it('changes no transport, subscription, or publication behavior when an outbox is declared', async () => {
    const OrderPlaced = topic('order.placed', { id: Uuid });
    const OrderRelayed = topic('order.relayed', { id: Uuid });

    const trace = async (declareOutbox: boolean): Promise<string[]> => {
      const calls: string[] = [];
      const transport: Transport = {
        publish: async (name: string, envelope: Envelope) => {
          calls.push(`publish:${name}:${envelope.topic}`);
        },
        subscribe: async (definition) => {
          calls.push(`subscribe:${definition.topic.name}:${definition.options.distribution}`);
        },
        start: async () => {
          calls.push('start');
        },
        stop: async () => {
          calls.push('stop');
        },
      };
      const plugin = new EventsPlugin({
        autoScan: false,
        transport,
        handlers: [handler(OrderPlaced).handle(async () => {})],
        ...(declareOutbox
          ? {
              outboxes: [
                outbox('orders', {
                  topics: [OrderPlaced, OrderRelayed],
                  table: 'orders.event_outbox',
                  datasource: 'orders',
                }),
              ],
            }
          : {}),
      });

      // biome-ignore lint/suspicious/noExplicitAny: the lifecycle hooks only need a module stand-in
      const app = {} as any;
      await plugin.warmup(app);
      await plugin.start(app);
      await plugin.stop(app);
      return calls;
    };

    const withoutOutbox = await trace(false);
    const withOutbox = await trace(true);

    expect(withoutOutbox).toEqual(['subscribe:order.placed:competing', 'start', 'stop']);
    expect(withOutbox).toEqual(withoutOutbox);
    // The declaration itself subscribes, relays, and publishes nothing.
    expect(withOutbox.some((call) => call.includes('order.relayed'))).toBe(false);
  });
});
