import { getExternalCaller, getLogger, getProjectRoot } from '@putnami/utils';
import type { BaseClient, ClientConfig, TransportMode } from './base-client';
import {
  type GeneratedClientDesign,
  type GeneratedClientUsageSource,
  recordGeneratedClientUsage,
} from './generated-client-design';

const logger = getLogger('client');

import { MAX_RETRIES_CAP, type RetryConfig } from './retry';
import { checkSpecDrift } from './spec-drift';
import type { Interceptor } from './transport.type';

/**
 * Builder for creating client instances with auto-negotiated transport.
 *
 * Instead of manually configuring transport, encoding, and proto metadata,
 * the builder auto-detects the best settings:
 *
 * 1. If the generated client has proto metadata → uses Connect + binary proto
 * 2. If Connect is unavailable → falls back to HTTP/JSON
 * 3. If a spec hash is embedded → runs drift detection at build time
 *
 * @example
 * ```typescript
 * const users = await ClientBuilder.for(UsersClient)
 *   .baseUrl('http://users-api:3000')
 *   .build();
 *
 * // With all options
 * const orders = await ClientBuilder.for(OrdersClient)
 *   .baseUrl('http://orders-api:3000')
 *   .clientId('frontend-bff')
 *   .timeout(10_000)
 *   .retry({ maxRetries: 5 })
 *   .interceptors([circuitBreakerInterceptor()])
 *   .checkDrift()
 *   .build();
 * ```
 */
export class ClientBuilder<T extends BaseClient> {
  private readonly ClientClass: ClientConstructor<T>;
  private config: Partial<BuilderConfig> = {};
  private shouldCheckDrift = false;
  private readonly usageSource?: GeneratedClientUsageSource;

  private constructor(ClientClass: ClientConstructor<T>, usageSource?: GeneratedClientUsageSource) {
    this.ClientClass = ClientClass;
    this.usageSource = usageSource;
  }

  /**
   * Start building a client for the given class.
   */
  static for<T extends BaseClient>(ClientClass: ClientConstructor<T>): ClientBuilder<T> {
    const classWithMeta = ClientClass as ClientClassWithMeta;
    const caller = classWithMeta.design ? getExternalCaller(getProjectRoot()) : undefined;
    return new ClientBuilder(
      ClientClass,
      caller
        ? {
            path: caller.filePath,
            line: caller.lineNumber,
            ...(caller.functionName ? { symbol: caller.functionName } : {}),
          }
        : undefined,
    );
  }

  /** Set the service base URL */
  baseUrl(url: string): this {
    this.config.baseUrl = url;
    return this;
  }

  /** Set the client identity for X-Client-Id header */
  clientId(id: string): this {
    this.config.clientId = id;
    return this;
  }

  /** Set request timeout in ms. Must be a positive number. */
  timeout(ms: number): this {
    if (!Number.isFinite(ms) || ms <= 0) {
      throw new Error(`ClientBuilder.timeout: ms must be a positive number, got ${ms}`);
    }
    this.config.timeoutMs = ms;
    return this;
  }

  /** Configure retry behavior. `maxRetries` is bounded to 0..MAX_RETRIES_CAP. */
  retry(config: Partial<RetryConfig>): this {
    if (config.maxRetries !== undefined) {
      if (!Number.isFinite(config.maxRetries) || config.maxRetries < 0) {
        throw new Error(`ClientBuilder.retry: maxRetries must be >= 0, got ${config.maxRetries}`);
      }
      if (config.maxRetries > MAX_RETRIES_CAP) {
        throw new Error(`ClientBuilder.retry: maxRetries must be <= ${MAX_RETRIES_CAP}, got ${config.maxRetries}`);
      }
    }
    this.config.retry = config;
    return this;
  }

  /** Add custom interceptors */
  interceptors(interceptors: Interceptor[]): this {
    this.config.interceptors = interceptors;
    return this;
  }

  /** Enable spec drift detection at startup */
  checkDrift(): this {
    this.shouldCheckDrift = true;
    return this;
  }

  /**
   * Build the client with auto-negotiated transport.
   *
   * Resolution order:
   * 1. If generated client has PROTO_META → Connect + proto binary
   * 2. Otherwise → HTTP/JSON
   * 3. If specHash is present and checkDrift() was called → warns on drift
   */
  async build(): Promise<T> {
    // Detect transport from the generated client class
    const transport = await this.negotiateTransport();
    return this.createClient(transport);
  }

  /**
   * Build the client synchronously (skip transport negotiation).
   * Uses the generated client's embedded metadata to pick the best transport.
   */
  buildSync(): T {
    const transport = this.detectTransportFromMeta();
    return this.createClient(transport);
  }

  /**
   * Shared client construction — assembles config, instantiates, and starts drift check.
   */
  private createClient(transport: TransportMode): T {
    if (!this.config.baseUrl) {
      throw new Error('ClientBuilder: baseUrl is required');
    }

    // Auto-extract packageName from class metadata for connect transport
    const classWithMeta = this.ClientClass as ClientClassWithMeta;
    const clientConfig: ClientConfig = {
      baseUrl: this.config.baseUrl,
      transport,
      packageName: classWithMeta.packageName,
      clientId: this.config.clientId,
      timeoutMs: this.config.timeoutMs,
      retry: this.config.retry,
      interceptors: this.config.interceptors,
    };

    const client = new this.ClientClass(clientConfig);
    if (classWithMeta.design) {
      recordGeneratedClientUsage(classWithMeta.design, this.usageSource);
    }

    // Run drift detection in background (non-blocking)
    if (this.shouldCheckDrift) {
      const specHash = (this.ClientClass as ClientClassWithMeta).specHash;
      if (specHash) {
        checkSpecDrift(this.config.baseUrl, specHash, transport).catch((error) => {
          logger.debug(
            `[client:builder] Unexpected error in spec drift check: ${error instanceof Error ? error.message : String(error)}`,
          );
        });
      }
    }

    return client;
  }

  /**
   * Negotiate the best transport by probing the service.
   * Tries Connect first (/_/api.proto), falls back to HTTP.
   */
  private async negotiateTransport(): Promise<TransportMode> {
    // If the generated client has proto metadata, it was generated from a proto spec
    const meta = this.detectTransportFromMeta();
    if (meta === 'connect') {
      // Verify the service actually supports Connect
      try {
        const response = await fetch(`${this.config.baseUrl}/_/api.proto`, {
          method: 'HEAD',
          signal: AbortSignal.timeout(3000),
        });
        if (response.ok) {
          return 'connect';
        }
      } catch {
        // Connect not available, fall back to HTTP
        logger.warn(
          `[client:builder] Connect transport not available at ${this.config.baseUrl}, falling back to HTTP/JSON`,
        );
      }
      return 'http';
    }
    return meta;
  }

  /**
   * Detect transport from the generated client's static metadata.
   */
  private detectTransportFromMeta(): TransportMode {
    const classWithMeta = this.ClientClass as ClientClassWithMeta;
    if (classWithMeta.packageName) {
      // Proto-generated clients have packageName (OpenAPI clients don't)
      return 'connect';
    }
    return 'http';
  }
}

interface BuilderConfig {
  baseUrl: string;
  clientId: string;
  timeoutMs: number;
  retry: Partial<RetryConfig>;
  interceptors: Interceptor[];
}

type ClientConstructor<T extends BaseClient> = new (config: ClientConfig) => T;

interface ClientClassWithMeta {
  specHash?: string;
  packageName?: string;
  design?: GeneratedClientDesign;
}
