import type { ClientContractDocument, ClientContractOperation, ClientSchema } from '@putnami/application';
import { type InferConfig, type Registration, provide } from '@putnami/runtime';
import { contextInterceptor } from '../interceptors/context.interceptor';
import { telemetryInterceptor } from '../interceptors/telemetry.interceptor';
import { type GeneratedClientsConfig, generatedClientsConfigToken } from '../config/client.config';
import type { BaseClient, ClientConfig, TransportMode } from './base-client';
import { type CredentialBinding, CredentialManager, serviceAuthInterceptor } from './credential';
import { ClientServiceConfigError } from './errors';
import { ServiceResponseCache, serviceResponseCacheInterceptor } from './response-cache';
import {
  assertNoCredentialHeaderConflict,
  type BindingHeaders,
  bindingHeadersInterceptor,
  snapshotBindingHeaders,
} from './service-headers';
import { serviceCircuitInterceptor, serviceDeadlineInterceptor } from './service-resilience';
import { serviceAttemptTelemetryInterceptor, serviceCallTelemetryInterceptor } from './service-telemetry';
import { parseServiceUrl } from './url';

/** Immutable metadata emitted into each generated first-party service client. */
export interface GeneratedServiceDescriptor {
  contract: ClientContractDocument;
  service: string;
  operations: Readonly<Record<string, ClientContractOperation>>;
  schemas?: Readonly<Record<string, ClientSchema>>;
  transport: TransportMode;
  packageName?: string;
}

/** Deployment values for one generated service. Secrets never enter generated code. */
export interface ServiceBinding {
  url: string;
  /** Caller-owned routing for fixed unary REST operations; never credential authority. */
  operationPaths?: Readonly<Record<string, string>>;
  clientId?: string;
  credentials?: Readonly<Record<string, CredentialBinding>>;
  /**
   * Static, non-secret request defaults, such as `X-Putnami-Observed-Revision`.
   * Explicit operation headers win, compared case-insensitively. Credential,
   * identity, request-context, tracing, origin and transport headers are
   * reserved, and so is every header a provider credential profile declares:
   * secrets belong in `credentials`. The binding snapshots this map, so a later
   * change to the caller's object has no effect.
   */
  headers?: Readonly<Record<string, string>>;
  allowInsecure?: boolean;
  /**
   * Carry the provider's free-text error `message` onto the thrown
   * `ClientFrameworkError`, redacted of this call's own credential material.
   * `false` by default: the envelope's prose is written for the human who made
   * the request, so a consumer that only logs or forwards a failure keeps the
   * local synthetic message and widens nothing. Set it on the deployment that
   * displays a provider error to the request's author; that consumer then owns
   * what it logs. The envelope's `error` member is never carried. See ADR 0006
   * of the client contract.
   */
  carryRemoteMessage?: boolean;
}

/** Minimal target implemented by Application and Module. */
export interface ServiceClientBindingTarget {
  register<T>(registration: Registration<T>): unknown;
  /** Application and Module expose their mounted application root. */
  getRoot?(): object;
}

export type GeneratedClientConstructor<T extends BaseClient> = new (config: ClientConfig) => T;

/**
 * What an application's registry owns for one service: the credential cache
 * and the response cache. Both live exactly as long as the last client bound
 * to that service in that application.
 */
interface CredentialManagerEntry {
  readonly manager: CredentialManager;
  readonly responses: ServiceResponseCache;
  references: number;
}

interface CredentialManagerLease {
  readonly manager: CredentialManager;
  readonly responses: ServiceResponseCache;
  release(): void;
}

const applicationCredentialManagers = new WeakMap<object, Map<string, CredentialManagerEntry>>();

/** Register a generated singleton. Generated helpers are typed wrappers around this. */
export function registerServiceClient<T extends BaseClient, TTarget extends ServiceClientBindingTarget>(
  target: TTarget,
  ClientClass: GeneratedClientConstructor<T>,
  descriptor: GeneratedServiceDescriptor,
  binding?: ServiceBinding,
): TTarget {
  target.register(
    createManagedServiceClientRegistration(ClientClass, descriptor, binding, (serviceId) =>
      credentialManagerLeaseFor(target.getRoot?.() ?? target, serviceId),
    ),
  );
  return target;
}

/** Low-level registration form retained for tests and custom composition. */
export function createServiceClientRegistration<T extends BaseClient>(
  ClientClass: GeneratedClientConstructor<T>,
  inputDescriptor: GeneratedServiceDescriptor,
  inputBinding?: ServiceBinding,
  manager = new CredentialManager(),
  responses = new ServiceResponseCache(),
): Registration<T> {
  return createManagedServiceClientRegistration(ClientClass, inputDescriptor, inputBinding, () => ({
    manager,
    responses,
    release: () => {},
  }));
}

function createManagedServiceClientRegistration<T extends BaseClient>(
  ClientClass: GeneratedClientConstructor<T>,
  inputDescriptor: GeneratedServiceDescriptor,
  inputBinding: ServiceBinding | undefined,
  acquireManager: (serviceId: string) => CredentialManagerLease,
): Registration<T> {
  const descriptor = immutableDescriptor(inputDescriptor);
  validateOperationPaths(descriptor, inputBinding?.operationPaths);
  const override = inputBinding ? resolveBoundBinding(descriptor, snapshotBinding(inputBinding)) : undefined;
  const configToken = override ? undefined : generatedClientsConfigToken();
  let managerLease: CredentialManagerLease | undefined;
  return provide(
    ClientClass,
    (resolve) => {
      const configured =
        override ?? resolveConfiguredBinding(resolve(configToken ?? generatedClientsConfigToken()), descriptor);
      validateOperationPaths(descriptor, configured.operationPaths);
      // Explicit path bindings own their caches. Two owner paths must never
      // share a cached page through the application service-id registry.
      managerLease = configured.operationPaths ? privateManagerLease() : acquireManager(descriptor.contract.service.id);
      return instantiateBoundClient(ClientClass, descriptor, configured, managerLease);
    },
    {
      scope: 'singleton',
      deps: configToken ? [configToken] : [],
      depsComplete: true,
      onClose: (client) => {
        try {
          client.dispose();
        } finally {
          managerLease?.release();
          managerLease = undefined;
        }
      },
    },
  );
}

function instantiateBoundClient<T extends BaseClient>(
  ClientClass: GeneratedClientConstructor<T>,
  descriptor: GeneratedServiceDescriptor,
  configured: ResolvedBinding,
  lease: CredentialManagerLease,
): T {
  const createClient = (selected: ResolvedBinding): T => {
    // First, even before the response cache: the key and every carrier see the
    // effective headers, and the call's own request is the only one changed.
    const bindingHeaders = selected.headers ? [bindingHeadersInterceptor(selected.headers)] : [];
    const authentication = serviceAuthInterceptor({
      serviceId: descriptor.contract.service.id,
      serviceUrl: selected.url,
      audience: descriptor.contract.service.audience,
      clientId: selected.clientId,
      profiles: descriptor.contract.credentials,
      credentials: selected.credentials,
      manager: lease.manager,
    });
    return new ClientClass({
      baseUrl: selected.url,
      operationPaths: selected.operationPaths,
      endpointBinding: (endpoint) => {
        const target = resolveBinding(descriptor.contract.service.id, { ...configured, url: endpoint });
        return { url: target.url, create: () => createClient(target) };
      },
      transport: descriptor.transport,
      packageName: descriptor.packageName,
      serviceId: descriptor.contract.service.id,
      carryRemoteMessage: selected.carryRemoteMessage,
      operationContracts: descriptor.operations,
      clientDefaults: descriptor.contract.defaults?.resilience,
      clientSchemas: descriptor.schemas,
      maxResponseSize: 32 * 1024 * 1024,
      interceptors: [
        ...bindingHeaders,
        // Next: a fresh answer costs no deadline, credential, breaker or
        // retry, and a stale one masks their failure (ADR 0007 of
        // protocols/clientcontract).
        serviceResponseCacheInterceptor(descriptor.contract.service.id, lease.responses, selected.url),
        serviceCallTelemetryInterceptor(descriptor.contract.service.id),
        telemetryInterceptor(descriptor.contract.service.id),
        serviceDeadlineInterceptor(),
        contextInterceptor(),
        serviceCircuitInterceptor(descriptor.contract.service.id),
      ],
      attemptInterceptors: [serviceAttemptTelemetryInterceptor(descriptor.contract.service.id), authentication],
      streamInterceptors: [...bindingHeaders, contextInterceptor(), authentication],
    });
  };
  try {
    return createClient(configured);
  } catch (error) {
    lease.release();
    throw error;
  }
}

function credentialManagerLeaseFor(applicationRoot: object, serviceId: string): CredentialManagerLease {
  let services = applicationCredentialManagers.get(applicationRoot);
  if (!services) {
    services = new Map();
    applicationCredentialManagers.set(applicationRoot, services);
  }
  let entry = services.get(serviceId);
  if (!entry) {
    entry = { manager: new CredentialManager(), responses: new ServiceResponseCache(), references: 0 };
    services.set(serviceId, entry);
  }
  entry.references++;
  let released = false;
  return {
    manager: entry.manager,
    responses: entry.responses,
    release: () => {
      if (released) return;
      released = true;
      entry.references--;
      if (entry.references > 0) return;
      entry.manager.dispose();
      entry.responses.dispose();
      services.delete(serviceId);
      if (services.size === 0) applicationCredentialManagers.delete(applicationRoot);
    },
  };
}

interface ResolvedBinding {
  url: string;
  operationPaths?: Readonly<Record<string, string>>;
  clientId: string;
  credentials: Readonly<Record<string, CredentialBinding>>;
  headers?: BindingHeaders;
  carryRemoteMessage: boolean;
  allowInsecure: boolean;
}

/**
 * Resolve a binding against its descriptor: a static header that names a
 * provider-declared credential header is refused here, once per binding.
 */
function resolveBoundBinding(descriptor: GeneratedServiceDescriptor, binding: ServiceBinding): ResolvedBinding {
  const resolved = resolveBinding(descriptor.contract.service.id, binding);
  assertNoCredentialHeaderConflict(resolved.headers, descriptor.contract.credentials);
  return resolved;
}

function resolveBinding(serviceId: string, binding: ServiceBinding): ResolvedBinding {
  const rawUrl = binding.url;
  let url: string;
  try {
    url = parseServiceUrl(rawUrl, { source: `service ${serviceId} URL`, allowInsecure: binding.allowInsecure });
  } catch (error) {
    throw new ClientServiceConfigError(error instanceof Error ? error.message : `service ${serviceId} URL is invalid`);
  }
  const clientId = binding.clientId;
  if (!clientId || !/^[A-Za-z0-9._-]+$/.test(clientId)) {
    throw new ClientServiceConfigError(`service ${serviceId} requires a valid client identity`);
  }
  const headers = snapshotBindingHeaders(binding.headers);
  return {
    url,
    operationPaths: binding.operationPaths,
    clientId,
    credentials: binding.credentials ?? Object.freeze({}),
    ...(headers && Object.keys(headers).length > 0 ? { headers } : {}),
    carryRemoteMessage: binding.carryRemoteMessage === true,
    allowInsecure: binding.allowInsecure === true,
  };
}

function snapshotBinding(binding: ServiceBinding): ServiceBinding {
  const credentials = Object.fromEntries(
    Object.entries(binding.credentials ?? {}).map(([profile, credential]) => [
      profile,
      deepFreeze({
        ...credential,
        ...(credential.parameters ? { parameters: { ...credential.parameters } } : {}),
      }),
    ]),
  );
  return deepFreeze({
    ...binding,
    ...(binding.operationPaths ? { operationPaths: { ...binding.operationPaths } } : {}),
    ...(binding.headers ? { headers: { ...binding.headers } } : {}),
    credentials,
  });
}

function resolveConfiguredBinding(
  config: InferConfig<typeof GeneratedClientsConfig>,
  descriptor: GeneratedServiceDescriptor,
): ResolvedBinding {
  const serviceId = descriptor.contract.service.id;
  const service = config.services[serviceId];
  if (!service) throw new ClientServiceConfigError(`service ${serviceId} has no configured binding`);
  return resolveBoundBinding(descriptor, {
    url: service.url,
    clientId: config.clientId,
    allowInsecure: service.allowInsecure,
    carryRemoteMessage: service.carryRemoteMessage,
    headers: service.headers,
    credentials: Object.fromEntries(
      Object.entries(service.credentials ?? {}).map(([profile, binding]) => [
        profile,
        normalizeConfiguredCredential(profile, binding),
      ]),
    ),
  });
}

function normalizeConfiguredCredential(
  profile: string,
  binding: InferConfig<typeof GeneratedClientsConfig>['services'][string]['credentials'] extends
    | Record<string, infer T>
    | undefined
    ? T
    : never,
): CredentialBinding {
  const sources = new Set([
    'oauth-client-credentials',
    'oauth-client-assertion',
    'oauth-extension-grant',
    'gcp-id-token',
    'forwarded-user',
    'forwarded-user-token',
    'static',
  ]);
  if (!sources.has(binding.source)) {
    throw new ClientServiceConfigError(`credential profile ${profile} has unsupported source ${binding.source}`);
  }
  if (binding.assertionSource !== undefined && binding.assertionSource !== 'gcp-id-token') {
    throw new ClientServiceConfigError(`credential profile ${profile} has unsupported assertion source`);
  }
  if (binding.tokenRequestFormat !== undefined && !['form', 'json'].includes(binding.tokenRequestFormat)) {
    throw new ClientServiceConfigError(`credential profile ${profile} has unsupported token request format`);
  }
  return {
    ...binding,
    source: binding.source as CredentialBinding['source'],
    assertionSource: binding.assertionSource as CredentialBinding['assertionSource'],
    tokenRequestFormat: binding.tokenRequestFormat as CredentialBinding['tokenRequestFormat'],
  };
}

function immutableDescriptor(descriptor: GeneratedServiceDescriptor): GeneratedServiceDescriptor {
  if (descriptor.contract.protocolVersion !== 1) {
    throw new ClientServiceConfigError(`unsupported generated client protocol ${descriptor.contract.protocolVersion}`);
  }
  return deepFreeze(structuredClone(descriptor));
}

function deepFreeze<T>(value: T): T {
  if (value && typeof value === 'object' && !Object.isFrozen(value)) {
    Object.freeze(value);
    for (const child of Object.values(value as Record<string, unknown>)) deepFreeze(child);
  }
  return value;
}

/** Bind caller-resolved endpoints without DI. Reuse through a page sequence, then dispose. */
export function bindServiceClient<T extends BaseClient>(
  ClientClass: GeneratedClientConstructor<T>,
  inputDescriptor: GeneratedServiceDescriptor,
  inputBinding: ServiceBinding,
): T {
  const descriptor = immutableDescriptor(inputDescriptor);
  validateOperationPaths(descriptor, inputBinding.operationPaths);
  const configured = resolveBoundBinding(descriptor, snapshotBinding(inputBinding));
  const lease = privateManagerLease();
  const client = instantiateBoundClient(ClientClass, descriptor, configured, lease);
  const dispose = client.dispose.bind(client);
  client.dispose = () => {
    try {
      dispose();
    } finally {
      lease.release();
    }
  };
  return client;
}

function validateOperationPaths(
  descriptor: GeneratedServiceDescriptor,
  paths?: Readonly<Record<string, string>>,
): void {
  for (const [id, path] of Object.entries(paths ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))) {
    const operation = Object.hasOwn(descriptor.operations, id) ? descriptor.operations[id] : undefined;
    if (!operation) throw new ClientServiceConfigError(`operation path names unknown operation ${id}`);
    const transport = operation.transports[0];
    if (
      operation.stream !== 'unary' ||
      operation.transports.length !== 1 ||
      transport?.protocol !== 'rest-json' ||
      transport.encoding !== 'json' ||
      /[{}]/.test(transport.path)
    ) {
      throw new ClientServiceConfigError(`operation path ${id} requires one fixed unary REST JSON transport`);
    }
    if (
      !path.startsWith('/') ||
      path.includes('//') ||
      /[?#\\{}%]/.test(path) ||
      [...path].some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127) ||
      path.split('/').some((part) => part === '.' || part === '..')
    ) {
      throw new ClientServiceConfigError(`operation path ${id} must be an unescaped absolute same-authority path`);
    }
  }
}

function privateManagerLease(): CredentialManagerLease {
  const manager = new CredentialManager();
  const responses = new ServiceResponseCache();
  return {
    manager,
    responses,
    release: () => {
      manager.dispose();
      responses.dispose();
    },
  };
}
