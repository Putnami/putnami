import { type InferConfig, useLogger } from '@putnami/runtime';
import type { PostgresConfig } from './config';
import { getGcpAccessToken, resolveGcpIdentity } from './gcp.utils';

/**
 * The connection options the package builds for one datasource, before the
 * physical pool turns them into `Bun.SQL` options (physical-pool.ts). Times
 * are in seconds.
 */
export interface ConnectionOptions {
  database: string;
  /** A hostname, or the directory of a Unix socket when it starts with `/`. */
  host: string;
  /** Omitted for a Unix socket. */
  port?: number;
  username: string;
  /** A fixed password, or a thunk the driver calls for each new connection. */
  password: string | (() => Promise<string>);
  /** Pool size. */
  max: number;
  connectTimeout: number;
  idleTimeout: number;
  /** Omitted when connections live for ever. */
  maxLifetime?: number;
  ssl?: 'require';
  /** Startup parameters sent with each connection. */
  connection?: Record<string, string>;
  /** Log each statement with its parameter values redacted (dev only). */
  debug?: boolean;
}

let identityResolver: () => Promise<string> = resolveGcpIdentity;
let tokenFetcher: () => Promise<string> = getGcpAccessToken;
let cachedIdentity: Promise<string> | undefined;

/**
 * Override the IAM identity resolver. Used by tests; production tries the
 * GCP metadata server first, then falls back to the gcloud CLI.
 */
export function __setIdentityResolverForTests(fn: (() => Promise<string>) | null): void {
  identityResolver = fn ?? resolveGcpIdentity;
  cachedIdentity = undefined;
}

/**
 * Override the IAM token fetcher. Used by tests; the production fetcher hits
 * the GCP metadata server.
 */
export function __setTokenFetcherForTests(fn: (() => Promise<string>) | null): void {
  tokenFetcher = fn ?? getGcpAccessToken;
}

function isUnixSocket(host: string): boolean {
  return host.startsWith('/');
}

function isLoopbackHost(host: string): boolean {
  return host === 'localhost' || host === '127.0.0.1' || host === '::1';
}

/**
 * Resolve whether TLS should be required. Defaults on for remote TCP hosts —
 * where plaintext exposes credentials and data on the wire — and off for
 * unix-socket / loopback hosts, which encrypt out of band or never leave the
 * machine. An explicit `ssl` config value always wins.
 */
function resolveSsl(host: string, explicit: boolean | undefined): boolean {
  if (explicit !== undefined) {
    return explicit;
  }
  return !isUnixSocket(host) && !isLoopbackHost(host);
}

function trimServiceAccountSuffix(user: string): string {
  return user.endsWith('.gserviceaccount.com') ? user.slice(0, -'.gserviceaccount.com'.length) : user;
}

async function resolveUser(): Promise<string> {
  if (!cachedIdentity) {
    cachedIdentity = identityResolver().then(trimServiceAccountSuffix);
  }
  try {
    return await cachedIdentity;
  } catch (err) {
    cachedIdentity = undefined;
    throw err;
  }
}

/**
 * Build the connection options for the configured database. When `user` is
 * empty, the active GCP identity is resolved (metadata server → gcloud).
 * When `password` is empty and `host` is a Unix socket (Cloud Run mounted
 * Cloud SQL), each connection fetches an IAM token from the metadata
 * server. Otherwise no password is sent — useful with a local
 * `cloud-sql-proxy` that handles auth itself.
 */
export async function buildOptions(config: InferConfig<typeof PostgresConfig>): Promise<ConnectionOptions> {
  const host = config.host;
  const explicitPassword = config.password?.trim();
  let username = config.user?.trim();
  if (!username) {
    username = await resolveUser();
    useLogger().debug(`database: auto-resolved user '${username}'`);
  } else {
    username = trimServiceAccountSuffix(username);
  }

  const options: ConnectionOptions = {
    database: config.database,
    host,
    username,
    password: '',
    max: config.poolSize,
    connectTimeout: config.connectTimeout,
    idleTimeout: config.idleTimeout,
    ...(config.maxLifetime > 0 ? { maxLifetime: config.maxLifetime } : {}),
  };

  if (!isUnixSocket(host)) {
    options.port = config.port;
  }

  if (explicitPassword) {
    options.password = explicitPassword;
  } else if (isUnixSocket(host)) {
    options.password = async () => {
      try {
        return await tokenFetcher();
      } catch (err) {
        const cause = err instanceof Error ? err.message : String(err);
        throw new Error(`database: failed to fetch IAM token: ${cause}`);
      }
    };
  } else {
    // TCP host with no configured password: pin the driver to a no-op thunk
    // so it can't fall back to a password from the environment. Harmless
    // under cloud-sql-proxy --auto-iam-authn (Cloud SQL replies
    // AuthenticationOk and the thunk is never invoked).
    options.password = async () => '';
  }

  if (resolveSsl(host, config.ssl)) {
    options.ssl = 'require';
  }

  if (config.debug) {
    // The client logs each statement with every parameter value redacted
    // (postgres/sql-query.ts): a password, a token or a Sensitive() value
    // never reaches the log. `debug` is a dev-only switch.
    options.debug = true;
  }

  const connection: Record<string, string> = {};
  if (config.statementTimeoutMs) {
    connection['statement_timeout'] = String(config.statementTimeoutMs);
  }
  if (config.idleInTransactionTimeoutMs) {
    connection['idle_in_transaction_session_timeout'] = String(config.idleInTransactionTimeoutMs);
  }
  if (Object.keys(connection).length > 0) {
    options.connection = connection;
  }

  return options;
}

/**
 * applyConnectionParams adds the extra startup parameters carried by a binding
 * (e.g. `application_name`) to already-built connection options. The driver
 * sends `connection` entries as startup parameters.
 *
 * `search_path` is NOT a startup parameter the connection factory sets any
 * more: several datasources share one physical pool, which has no single
 * `search_path`, so the datasource-bound client sets it per operation instead
 * (see postgres/datasource-client.ts and doc/adr/0003). The `searchPath`
 * argument remains for callers that render one datasource's options on their
 * own; the factory passes `undefined`.
 */
export function applyConnectionParams(
  options: ConnectionOptions,
  searchPath?: string,
  extra?: Record<string, string>,
): void {
  const connection: Record<string, string> = { ...options.connection };
  if (extra) {
    for (const [k, v] of Object.entries(extra)) {
      connection[k] = v;
    }
  }
  if (searchPath) {
    connection['search_path'] = searchPath;
  }
  if (Object.keys(connection).length > 0) {
    options.connection = connection;
  }
}
