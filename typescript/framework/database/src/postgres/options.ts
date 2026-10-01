import { type InferConfig, REDACTED, useLogger } from '@putnami/runtime';
import postgres from 'postgres';
import type { PostgresConfig } from './config';
import { getGcpAccessToken, resolveGcpIdentity } from './gcp.utils';
import { TYPES } from './type-mapping';

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
 * Build postgres.js options for the configured database. When `user` is
 * empty, the active GCP identity is resolved (metadata server → gcloud).
 * When `password` is empty and `host` is a Unix socket (Cloud Run mounted
 * Cloud SQL), each connection fetches an IAM token from the metadata
 * server. Otherwise no password is sent — useful with a local
 * `cloud-sql-proxy` that handles auth itself.
 */
export async function buildOptions(
  config: InferConfig<typeof PostgresConfig>,
): Promise<postgres.Options<Record<string, postgres.PostgresType>>> {
  const host = config.host;
  const explicitPassword = config.password?.trim();
  let username = config.user?.trim();
  if (!username) {
    username = await resolveUser();
    useLogger().debug(`database: auto-resolved user '${username}'`);
  } else {
    username = trimServiceAccountSuffix(username);
  }

  const options: postgres.Options<Record<string, postgres.PostgresType>> = {
    database: config.database,
    host,
    username,
    max: config.poolSize,
    onnotice: () => {},
    fetch_types: true,
    types: TYPES,
    transform: {
      undefined: null,
      ...postgres.camel,
    },
    connect_timeout: config.connectTimeout,
    idle_timeout: config.idleTimeout,
    ...(config.maxLifetime > 0 ? { max_lifetime: config.maxLifetime } : {}),
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
    // TCP host with no configured password: pin postgres.js to a no-op thunk
    // so it can't fall back to PGPASSWORD from the shell. Harmless under
    // cloud-sql-proxy --auto-iam-authn (Cloud SQL replies AuthenticationOk and
    // the thunk is never invoked); avoids leaking a stray env var if the
    // server does issue a password challenge.
    options.password = async () => '';
  }

  if (resolveSsl(host, config.ssl)) {
    options.ssl = 'require';
  }

  if (config.debug) {
    // The postgres.js debug callback only sees positional parameter values, with
    // no link back to their columns — so a password/token or any Sensitive()
    // value would be logged in plaintext. The parameterized query text and types
    // are enough to debug a statement, so redact every value by default. `debug`
    // is a dev-only switch; never enable it in production.
    options.debug = (_connection, query, parameters, paramTypes) => {
      useLogger().debug('postgres query', {
        query,
        parameters: Array.isArray(parameters) ? parameters.map(() => REDACTED) : parameters,
        paramTypes,
      });
    };
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
 * (e.g. `application_name`) to already-built postgres.js options. postgres.js
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
  options: postgres.Options<Record<string, postgres.PostgresType>>,
  searchPath?: string,
  extra?: Record<string, string>,
): void {
  const connection: Record<string, string> = { ...(options.connection as Record<string, string> | undefined) };
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
