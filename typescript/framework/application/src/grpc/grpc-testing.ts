import { readEnvelope } from './proto-codec';

/**
 * gRPC Test Client
 *
 * Provides a lightweight test client for calling gRPC services over the
 * Connect protocol. Designed for use with Bun's native test runner.
 *
 * Features:
 * - Unary RPC calls (JSON and binary proto)
 * - Server streaming with automatic message collection
 * - Service discovery via gRPC reflection
 * - Configurable headers, timeouts, and content types
 *
 * @example
 * ```typescript
 * import { createGrpcTestClient } from '@putnami/application';
 *
 * const client = createGrpcTestClient({
 *   baseUrl: `http://localhost:${server.port}`,
 *   packageName: 'myapp.v1',
 * });
 *
 * const res = await client.unary('UsersService/ListUsers', { page: 1 });
 * expect(res.data.users).toHaveLength(10);
 *
 * const services = await client.listServices();
 * expect(services).toContain('myapp.v1.UsersService');
 *
 * await client.close();
 * ```
 * @module
 */

export interface GrpcTestClient {
  /**
   * Make a unary gRPC call via Connect JSON.
   *
   * @param path - RPC path: `"Service/Method"` or fully qualified `"pkg.Service/Method"`
   * @param data - Request payload (JSON object)
   * @param options - Optional headers, timeout, content type
   */
  unary<T = unknown>(
    path: string,
    data?: Record<string, unknown>,
    options?: GrpcCallOptions,
  ): Promise<GrpcTestResponse<T>>;

  /**
   * Make a server-streaming gRPC call via Connect streaming protocol.
   * Collects all streamed messages and returns them as an array.
   *
   * @param path - RPC path: `"Service/Method"` or fully qualified
   * @param data - Request payload
   */
  serverStream<T = unknown>(
    path: string,
    data?: Record<string, unknown>,
    options?: GrpcCallOptions,
  ): Promise<GrpcStreamResult<T>>;

  /**
   * List all gRPC services via the reflection API.
   * Returns fully-qualified service names (e.g. `["myapp.v1.UsersService"]`).
   */
  listServices(): Promise<string[]>;

  /**
   * Check the gRPC health status.
   * Returns the serving status number (1 = SERVING).
   */
  checkHealth(): Promise<number>;

  /** Close the test client (no-op for HTTP, included for interface consistency). */
  close(): Promise<void>;
}

export interface GrpcCallOptions {
  /** Override Content-Type (default: 'application/json') */
  contentType?: string;
  /** Additional request headers */
  headers?: Record<string, string>;
  /** Request timeout in milliseconds */
  timeout?: number;
}

export interface GrpcTestResponse<T = unknown> {
  /** Parsed response data */
  data: T;
  /** HTTP status code */
  status: number;
  /** Response headers */
  headers: Headers;
}

export interface GrpcStreamResult<T = unknown> {
  /** Collected stream messages */
  messages: T[];
  /** End-of-stream trailers (gRPC status, error details) */
  trailers: Record<string, unknown>;
}

export interface GrpcTestClientOptions {
  /** Base URL of the gRPC server (e.g. `http://localhost:3000`) */
  baseUrl: string;
  /** Proto package name — used to expand short paths like `"Service/Method"` */
  packageName?: string;
}

/**
 * Create a gRPC test client for calling services over the Connect protocol.
 *
 * The client makes real HTTP calls — no mocking. Use it for integration
 * testing against a running server (e.g. started in a `beforeAll` hook).
 *
 * @example
 * ```typescript
 * const client = createGrpcTestClient({
 *   baseUrl: `http://localhost:${server.port}`,
 *   packageName: 'myapp.v1',
 * });
 *
 * // Short path (expanded with packageName)
 * const res = await client.unary('UsersService/ListUsers', { page: 1 });
 *
 * // Fully-qualified path
 * const res2 = await client.unary('myapp.v1.UsersService/ListUsers');
 *
 * await client.close();
 * ```
 */
export function createGrpcTestClient(options: GrpcTestClientOptions): GrpcTestClient {
  const { baseUrl, packageName } = options;

  function resolvePath(path: string): string {
    // Already fully-qualified (contains a dot before the slash)
    if (path.includes('.')) return `/${path}`;
    // Short path: prepend package name
    if (packageName) return `/${packageName}.${path}`;
    return `/${path}`;
  }

  return {
    async unary<T = unknown>(
      path: string,
      data?: Record<string, unknown>,
      callOptions?: GrpcCallOptions,
    ): Promise<GrpcTestResponse<T>> {
      const url = `${baseUrl}${resolvePath(path)}`;
      const headers: Record<string, string> = {
        'Content-Type': callOptions?.contentType ?? 'application/json',
        Accept: callOptions?.contentType ?? 'application/json',
        ...callOptions?.headers,
      };

      const controller = new AbortController();
      let timer: ReturnType<typeof setTimeout> | undefined;
      if (callOptions?.timeout) {
        timer = setTimeout(() => controller.abort(), callOptions.timeout);
      }

      try {
        const response = await fetch(url, {
          method: 'POST',
          headers,
          body: JSON.stringify(data ?? {}),
          signal: controller.signal,
        });

        const responseData = (await response.json()) as T;
        return {
          data: responseData,
          status: response.status,
          headers: response.headers,
        };
      } finally {
        if (timer) clearTimeout(timer);
      }
    },

    async serverStream<T = unknown>(
      path: string,
      data?: Record<string, unknown>,
      callOptions?: GrpcCallOptions,
    ): Promise<GrpcStreamResult<T>> {
      const url = `${baseUrl}${resolvePath(path)}`;
      const headers: Record<string, string> = {
        'Content-Type': callOptions?.contentType ?? 'application/json',
        Accept: 'application/connect+streaming+json',
        ...callOptions?.headers,
      };

      const controller = new AbortController();
      const timeout = callOptions?.timeout ?? 30_000;
      const timer = setTimeout(() => controller.abort(), timeout);

      let response: Response;
      try {
        response = await fetch(url, {
          method: 'POST',
          headers,
          body: JSON.stringify(data ?? {}),
          signal: controller.signal,
        });
      } finally {
        clearTimeout(timer);
      }

      if (!response.body) {
        return { messages: [], trailers: {} };
      }

      // Read the full response body as bytes then parse Connect envelopes
      const buffer = new Uint8Array(await response.arrayBuffer());
      const messages: T[] = [];
      let trailers: Record<string, unknown> = {};
      let offset = 0;

      while (offset < buffer.length) {
        const envelope = readEnvelope(buffer, offset);
        if (!envelope) break;
        offset += envelope.consumed;

        if (envelope.flags === 0x00 || envelope.flags === 0x01) {
          // Data frame (0x00 = uncompressed, 0x01 = compressed)
          try {
            const text = new TextDecoder().decode(envelope.payload);
            messages.push(JSON.parse(text) as T);
          } catch {
            // Skip malformed frames
          }
        } else if (envelope.flags === 0x02) {
          // End-of-stream trailers
          try {
            const text = new TextDecoder().decode(envelope.payload);
            trailers = JSON.parse(text) as Record<string, unknown>;
          } catch {
            // Skip malformed trailers
          }
        }
      }

      return { messages, trailers };
    },

    async listServices(): Promise<string[]> {
      const url = `${baseUrl}/grpc.reflection.v1.ServerReflection/ServerReflectionInfo`;
      const response = await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ listServices: '' }),
        signal: AbortSignal.timeout(30_000),
      });

      const body = (await response.json()) as {
        listServicesResponse?: { service?: { name: string }[] };
      };
      return body.listServicesResponse?.service?.map((s) => s.name) ?? [];
    },

    async checkHealth(): Promise<number> {
      const url = `${baseUrl}/grpc.health.v1.Health/Check`;
      const response = await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
        signal: AbortSignal.timeout(30_000),
      });

      const body = (await response.json()) as { status?: number };
      return body.status ?? 0;
    },

    async close(): Promise<void> {
      // No-op for HTTP-based client.
      // Included for interface consistency and future connection pooling.
    },
  };
}
