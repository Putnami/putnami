import type { useLogger } from '@putnami/runtime';
import { LocalServerTransport } from './server/local-server.transport';
import { EVENTS_DEFAULT_PORT, MemoryServer } from './server/memory-server';
import { EVENT_SERVER_TRANSPORT_KIND, eventServerTransport, routingTransport, type Transport } from './transport';
import type { EventServerTransportConfig } from './transport';
import type { EventsConfig } from './events.plugin';

// ---------------------------------------------------------------------------
// Transport resolution & config validation
//
// Picks the right transport from the plugin config (explicit, routed, remote
// endpoint, or local in-memory broker), split out of the plugin lifecycle.
// ---------------------------------------------------------------------------

type Logger = ReturnType<typeof useLogger>;

export interface ResolvedTransport {
  transport: Transport;
  /** Set only when this process owns a freshly created local events server. */
  memoryServer?: MemoryServer;
}

/**
 * Resolve the transport for the configured plugin:
 * - explicit `transport` wins,
 * - else a routing transport over named `transports`,
 * - else a remote `LocalServerTransport` for a configured endpoint,
 * - else a local in-memory broker (joining or starting the local server).
 */
export async function resolveTransport(config: EventsConfig, logger: Logger): Promise<ResolvedTransport> {
  if (config.transport && typeof config.transport !== 'string') {
    return { transport: config.transport };
  }

  if (config.transports) {
    return {
      transport: routingTransport({
        transports: config.transports,
        routes: config.routes,
        defaultTransport: config.defaultTransport,
      }),
    };
  }

  if (config.transport === EVENT_SERVER_TRANSPORT_KIND) {
    if (!config.eventServer) {
      throw new Error('events.eventServer config is required when events.transport is eventserver');
    }
    logger.debug(`Connecting to Event Server at ${config.eventServer.endpoint}...`);
    return { transport: eventServerTransport(config.eventServer as EventServerTransportConfig) };
  }

  if (typeof config.transport === 'string') {
    throw new Error(`Unsupported events.transport '${config.transport}'`);
  }

  const endpoint = config.endpoint ?? process.env['EVENTS_ENDPOINT'];
  if (endpoint) {
    const token = config.token ?? process.env['EVENTS_TOKEN'];
    logger.debug(`Connecting to events service at ${endpoint}...`);
    return { transport: new LocalServerTransport(endpoint, token, { drainTimeout: config.drainTimeout }) };
  }

  // Local dev: in-memory broker
  const port = config.port ?? EVENTS_DEFAULT_PORT;
  if (await MemoryServer.isRunning(port)) {
    return { transport: await createLocalServerTransport(port, config.drainTimeout, logger) };
  }

  // Start our own local server
  const memoryServer = new MemoryServer({
    port,
    drainTimeout: config.drainTimeout,
    simulateDuplicates: config.simulateDuplicates ?? false,
  });
  return { transport: memoryServer.getBroker(), memoryServer };
}

/** Connect to a local events server already running on `port`. */
export async function createLocalServerTransport(
  port: number,
  drainTimeout: number | undefined,
  logger: Logger,
): Promise<LocalServerTransport> {
  const authToken = await MemoryServer.readAuthToken(port);
  logger.debug(`Local events server already running on port ${port}, connecting...`);
  return new LocalServerTransport(`http://127.0.0.1:${port}`, authToken, { drainTimeout });
}

export function isAddressInUse(error: unknown): boolean {
  return (
    typeof error === 'object' &&
    error !== null &&
    'code' in error &&
    (error as { code?: unknown }).code === 'EADDRINUSE'
  );
}

export function validateTransportConfig(config: EventsConfig): void {
  if (config.transport && (config.transports || config.routes || config.defaultTransport)) {
    throw new Error('events() cannot combine transport with transports/routes/defaultTransport.');
  }

  if (!config.transports && (config.routes || config.defaultTransport)) {
    throw new Error('events() routes/defaultTransport require transports.');
  }
}
