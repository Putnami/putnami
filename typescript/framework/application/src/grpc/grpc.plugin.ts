import type { Type } from '@putnami/runtime';
import { useLogger } from '@putnami/runtime';
import { ApiPlugin } from '../api';
import type { Module, Plugin } from '../application';
import { HttpPlugin } from '../http/http.plugin';
import { HttpResponse } from '../http/http-response';
import type { HttpRequestContext } from '../http/http-context.type';
import { ProtoPlugin } from '../proto';
import { registerReflection } from './grpc-reflection';
import { protoEnumRegistry } from './proto-codec';
import {
  buildUnaryRpcHandler,
  buildStreamRpcHandler,
  buildUnimplementedRpcHandler,
  matchRpcToRoute,
} from './grpc-handlers';
import {
  CT_JSON,
  CT_PROTO,
  CT_CONNECT_STREAM_JSON,
  CT_CONNECT_STREAM_PROTO,
  CT_GRPC_WEB_JSON,
  CT_GRPC_WEB_PROTO,
  acceptContains,
  connectResponseHeaders,
} from './grpc-protocol';

export interface GrpcConfig {
  /**
   * Content types to accept for gRPC requests.
   * Defaults to Connect protocol (JSON + proto binary) and gRPC-Web JSON.
   */
  acceptContentTypes?: string[];
  /**
   * Enable gzip compression for gRPC responses.
   * When enabled, responses are compressed if the client advertises
   * gzip support via `grpc-accept-encoding` or `accept-encoding`.
   * Defaults to `true`.
   */
  compression?: boolean;
}

/**
 * gRPC plugin that serves API routes over gRPC on the **same HTTP port**.
 *
 * Supports:
 * - **Direct handler dispatch** — no internal server.fetch() loopback
 * - **Binary protobuf** — alongside Connect JSON (content-type negotiation)
 * - **Server streaming** — Connect streaming protocol with envelope framing
 * - **Client/bidi streaming** — answers gRPC status 12 UNIMPLEMENTED on the
 *   Connect route (HTTP/1.1 cannot carry client streams); the generated
 *   clients serve these modes over the WebSocket stream transport
 *
 * Registers POST routes at `/{package}.{Service}/{Method}` on the existing
 * HTTP server. Incoming gRPC/Connect requests are dispatched directly to the
 * API route handler — all endpoint-level middleware, validation, and auth apply.
 *
 * **Observability**: gRPC routes are registered on the HTTP server, so global
 * middleware (logger, telemetry, trace) runs on every gRPC request. The trace
 * correlation ID is propagated to the inner handler context so that logs
 * emitted inside handlers include the correct trace ID.
 *
 * Requires `ProtoPlugin` and `ApiPlugin` to be registered before this plugin.
 *
 * @example
 * ```typescript
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(proto({ packageName: 'myapp.v1' }))
 *   .use(grpc());
 * ```
 */
export class GrpcPlugin implements Plugin {
  private readonly config: Required<GrpcConfig>;
  private rpcCount = 0;
  /**
   * Serving state reported by the gRPC Health/Check endpoint. Becomes `true`
   * once warmup wires the routes and flips back to `false` on shutdown so the
   * health response reads NOT_SERVING while the app is draining.
   */
  private serving = false;

  constructor(config: Required<GrpcConfig>) {
    this.config = config;
  }

  /** Exact Connect payload encodings accepted by the configured route matcher. */
  connectEncodings(serverStreaming: boolean): Array<'proto' | 'json'> {
    const accepted = new Set(this.config.acceptContentTypes.map((contentType) => contentType.split(';')[0].trim()));
    // Unary calls negotiate `application/{proto,json}`; streams negotiate
    // `application/connect+{proto,json}`. A route that accepts neither cannot
    // advertise that encoding in `x-putnami-client`, which is what stops a
    // generated client attempting a transport this server will refuse.
    const proto = serverStreaming
      ? accepted.has(CT_CONNECT_STREAM_PROTO)
      : accepted.has(CT_PROTO) || accepted.has(CT_GRPC_WEB_PROTO);
    const json = serverStreaming
      ? accepted.has(CT_CONNECT_STREAM_JSON)
      : accepted.has(CT_JSON) || accepted.has(CT_GRPC_WEB_JSON);
    return [...(proto ? (['proto'] as const) : []), ...(json ? (['json'] as const) : [])];
  }

  async warmup(app: Module): Promise<void> {
    const logger = useLogger('putnami:grpc');

    const protoPlugin = this.findPlugin(app, ProtoPlugin);
    if (!protoPlugin) {
      logger.warn('GrpcPlugin requires ProtoPlugin — skipping gRPC setup');
      return;
    }

    const apiPlugin = this.findPlugin(app, ApiPlugin);
    if (!apiPlugin) {
      logger.warn('GrpcPlugin requires ApiPlugin — skipping gRPC setup');
      return;
    }

    const httpPlugin = await app.ensurePlugin(HttpPlugin);

    const proto = protoPlugin.proto();
    if (!proto) {
      logger.warn('No proto definition available — skipping gRPC setup');
      return;
    }

    const routes = [...apiPlugin.routes];
    const enums = protoEnumRegistry(proto.enumMeta ?? {});

    // Register a POST route on the HTTP server for each RPC
    for (const [serviceName, rpcNames] of Object.entries(proto.services)) {
      for (const rpcName of rpcNames) {
        const grpcPath = `/${proto.packageName}.${serviceName}/${rpcName}`;
        const route = matchRpcToRoute(rpcName, routes);

        if (!route) {
          continue;
        }

        // Resolve handler directly from the HTTP router (no server.fetch loopback)
        const dummyPath = route.path.replace(/\[([^\]]+)\]/g, '__grpc_placeholder__');
        const resolved = httpPlugin.resolveHandler(route.method, dummyPath);

        if (!resolved) {
          logger.warn(`gRPC: no handler found for ${route.method} ${route.path} — skipping ${rpcName}`);
          continue;
        }

        // Check if this is a streaming RPC
        const streamDef = route.streamMode ? apiPlugin.streamDefinitions.get(route.path) : undefined;

        const useCompression = this.config.compression;
        // Server streaming rides Connect streaming frames; client/bidi streams
        // cannot exist over HTTP/1.1 (Bun.serve's ceiling), so their RPCs
        // answer UNIMPLEMENTED instead of silently degrading to unary — the
        // generated clients carry those modes over the WebSocket transport.
        let handler: (ctx: HttpRequestContext) => Promise<HttpResponse>;
        if (streamDef?.mode === 'server') {
          handler = buildStreamRpcHandler(streamDef, route, proto.messageMeta, enums, useCompression);
        } else if (streamDef?.mode === 'client' || streamDef?.mode === 'bidirectional') {
          handler = buildUnimplementedRpcHandler(streamDef.mode);
        } else {
          handler = buildUnaryRpcHandler(resolved.handler, route, proto.messageMeta, enums, useCompression);
        }

        httpPlugin.route('POST', grpcPath, handler, {
          accept: this.config.acceptContentTypes,
        });
        this.rpcCount++;
      }
    }

    // Register gRPC Health Checking Protocol (grpc.health.v1.Health).
    // The handler reports the live serving state so load balancers drain gRPC
    // traffic during shutdown instead of seeing a permanent SERVING.
    registerHealthCheck(httpPlugin, this.config.acceptContentTypes, () => this.serving);

    // Register gRPC Server Reflection (v1 and v1alpha)
    registerReflection(httpPlugin, proto, this.config.acceptContentTypes);

    // gRPC is now wired and serving until shutdown.
    this.serving = true;

    if (this.rpcCount > 0) {
      logger.debug(`gRPC: registered ${this.rpcCount} RPCs (direct dispatch, Connect protocol)`);
    }
  }

  /** Flip the health endpoint to NOT_SERVING so gRPC traffic drains on shutdown. */
  async stop(): Promise<void> {
    this.serving = false;
  }

  private findPlugin<P extends Plugin>(app: Module, type: Type<P>): P | undefined {
    try {
      return app.getPlugin(type);
    } catch {
      return undefined;
    }
  }
}

// ---------------------------------------------------------------------------
// gRPC Health Checking Protocol (grpc.health.v1)
// ---------------------------------------------------------------------------

// ServingStatus enum values
const SERVING_STATUS = { UNKNOWN: 0, SERVING: 1, NOT_SERVING: 2 } as const;

function registerHealthCheck(httpPlugin: HttpPlugin, acceptContentTypes: string[], isServing: () => boolean): void {
  const healthPath = '/grpc.health.v1.Health/Check';

  httpPlugin.route(
    'POST',
    healthPath,
    async (ctx: HttpRequestContext) => {
      const contentType = ctx.headers.get('content-type') ?? CT_JSON;
      const acceptHeader = ctx.headers.get('accept') ?? contentType;
      const useBinary = acceptContains(acceptHeader, CT_PROTO);
      const status = isServing() ? SERVING_STATUS.SERVING : SERVING_STATUS.NOT_SERVING;

      if (useBinary) {
        // Proto response: field 1 (status) = varint, tag (1 << 3) | 0 = 0x08.
        const payload = new Uint8Array([0x08, status]);
        return new HttpResponse(payload.buffer.slice(0) as ArrayBuffer, {
          headers: { 'Content-Type': CT_PROTO, ...connectResponseHeaders() },
        });
      }

      return HttpResponse.json({ status }, { headers: connectResponseHeaders() });
    },
    { accept: acceptContentTypes },
  );
}

/**
 * Factory function for creating a GrpcPlugin.
 *
 * Registers gRPC routes on the same HTTP server — no extra port needed.
 * Uses the Connect protocol for maximum compatibility with single-port
 * environments like Cloud Run.
 *
 * Supports:
 * - **Connect JSON** (`application/json`) — default, human-readable
 * - **Connect binary** (`application/proto`) — protobuf wire format
 * - **Server streaming** — Connect streaming protocol with envelope framing
 *
 * Requires `ProtoPlugin` and `ApiPlugin` to be registered.
 *
 * @example
 * ```typescript
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(proto({ packageName: 'myapp.v1' }))
 *   .use(grpc());
 *
 * // Connect JSON: POST /myapp.v1.UsersService/ListUsers
 * //   Content-Type: application/json
 * // Connect binary: POST /myapp.v1.UsersService/ListUsers
 * //   Content-Type: application/proto
 * ```
 */
export function grpc(config: Partial<GrpcConfig> = {}): GrpcPlugin {
  return new GrpcPlugin({
    acceptContentTypes: config.acceptContentTypes ?? [
      CT_JSON,
      CT_PROTO,
      CT_GRPC_WEB_JSON,
      CT_GRPC_WEB_PROTO,
      CT_CONNECT_STREAM_JSON,
      CT_CONNECT_STREAM_PROTO,
    ],
    compression: config.compression ?? true,
  });
}
