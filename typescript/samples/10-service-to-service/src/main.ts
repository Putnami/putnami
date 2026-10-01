import { application, api, grpc, http, logger, openapi, platform, proto, redirect } from '@putnami/application';
import type { ServiceBinding } from '@putnami/client';
import { clientGenerator } from '@putnami/client/generator';
import { useRawConfigSection } from '@putnami/runtime';
import { registerBlobsClient, registerItemsClient, registerQuotesClient } from '../clients/ts/src';
import { TENANT_HEADER } from './caller-identity';
import { CATALOG_API_KEY, CATALOG_KEY_HEADER, catalogIdentityResolver } from './workload-identity';

/** The service this provider declares, and the one its own clients bind. */
const SERVICE_ID = 'catalog.items';

/** The port this sample serves by default: `options.serve.port` in putnami.json. */
const DEFAULT_PORT = 3910;

/**
 * The port this instance serves: the caller's, else `PORT`, else the sample's
 * declared one. `PORT=0` asks for an ephemeral port — what `putnami compose`
 * and `putnami qualify` pass so two instances never collide — and this
 * provider binds its own client before it listens, so it reserves the port now
 * rather than learning it at listen time. The ready marker still reports the
 * port the server bound.
 */
function resolvePort(requested?: number): number {
  const port = requested ?? Number(process.env['PORT'] ?? DEFAULT_PORT);
  if (Number.isInteger(port) && port > 0) return port;
  const reservation = Bun.serve({ port: 0, fetch: () => new Response(null, { status: 503 }) });
  const reserved: number = reservation.port ?? DEFAULT_PORT;
  reservation.stop(true);
  return reserved;
}

/**
 * This provider is also its own consumer: `/proxy` calls it through the
 * generated client. A deployment binds that client through the `clients`
 * block, like any consumer, and that binding wins. When none declares one —
 * `putnami serve .`, a composition that runs this provider alone — the
 * provider binds itself to the port it serves, with the key it accepts; no
 * consumer wrote a URL, and no other instance is reached.
 */
function selfBinding(port: number): ServiceBinding | undefined {
  const services = useRawConfigSection('clients')?.['services'];
  if (services && typeof services === 'object' && SERVICE_ID in services) return undefined;
  return {
    url: `http://localhost:${port}`,
    clientId: 'service-to-service-sample',
    allowInsecure: true,
    credentials: { 'catalog-key': { source: 'static', value: CATALOG_API_KEY } },
  };
}

export function app(options: { port?: number } = {}) {
  const port = resolvePort(options.port);
  const instance = application()
    .feature({
      id: 'items/manage',
      name: 'Item management',
      outcome: 'Consumers can list and retrieve catalog items through a typed client',
      owner: 'samples',
    })
    .use(
      http({ port })
        // Identity is established once, before every route, so an endpoint
        // only declares what it requires.
        .prepend(catalogIdentityResolver())
        .get('/', () => redirect('/items')),
    )
    .use(logger())
    .use(platform())
    .use(
      api({
        client: {
          service: { id: SERVICE_ID, audience: 'api://catalog.items' },
          // The watch stream declares this profile, so every generated client
          // carries the credential and no consumer writes an Authorization
          // header. An unbound profile makes the call fail before dispatch
          // instead of reaching the provider anonymously.
          credentials: {
            'catalog-key': { kind: 'api-key', header: CATALOG_KEY_HEADER },
            // A named secondary credential, required together with the api key
            // by /tenant-check.
            tenant: { kind: 'named-header', header: TENANT_HEADER },
            // The caller's own user token, forwarded from the consumer's
            // inbound request by /whoami. It is never cached nor minted.
            user: { kind: 'forwarded-user-token' },
          },
        },
      }),
    )
    // The Connect wire: the proto plugin publishes the descriptor that is the
    // protobuf codec, and the gRPC plugin serves every unary and server-stream
    // route on the same port. Mounted, it puts Connect first on every route
    // that declares no order, so the REST and stream routes declare theirs and
    // only the quotes routes declare Connect.
    .use(proto({ packageName: 'catalog.items.v1' }))
    .use(grpc())
    .use(
      openapi({
        title: 'Items Service',
        version: '1.0.0',
        exposeRoute: true,
        publicRoute: '/openapi.json',
      }),
    )
    .use(
      clientGenerator({
        packageName: '@example/items-client',
        // Both targets are synchronized mechanically from this provider-owned
        // contract by the workspace clientgen command.
        targets: ['ts', 'go'],
        go: {
          modulePath: 'go.putnami.dev/examples/ts-items-client',
          packageName: 'itemsclient',
          clientName: 'ItemsClient',
        },
      }),
    );

  const binding = selfBinding(port);
  registerItemsClient(instance, binding);
  // The raw octet operations group under their own path segment, so the
  // emitter renders a second client class from the same contract. Both bind to
  // the same declared service and the same credential profiles.
  registerBlobsClient(instance, binding);
  // The Connect-only quotes routes render a third class from the same contract.
  registerQuotesClient(instance, binding);
  return instance;
}
