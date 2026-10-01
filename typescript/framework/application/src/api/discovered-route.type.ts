import type { HttpMethod } from '../http/http-method.type';
import type { SchemaDefinition } from '@putnami/runtime';
import type { BinaryMeta } from './route/binary';
import type { ProviderWire } from './route/byte-stream';
import type { BodyContentType } from './route/endpoint.types';
import type { StreamMode } from './route/stream-endpoint';
import type { EndpointMeta, ResponseDeclarations } from './route/response-meta';

/** Describes a single registered route with its schemas. */
export interface DiscoveredRoute {
  method: HttpMethod;
  path: string;
  /** Present when the route is a stream endpoint (SSE / WebSocket). */
  streamMode?: StreamMode;
  /** Present when the stream speaks a provider-owned WebSocket wire instead of the first-party conversation. */
  wire?: ProviderWire;
  schemas?: {
    readonly params?: SchemaDefinition;
    readonly query?: SchemaDefinition;
    readonly body?: SchemaDefinition;
    /** Content type for the request body. Defaults to `'application/json'`. */
    readonly bodyContentType?: BodyContentType;
    /** Raw octet request payload: its media type and its byte bound. */
    readonly bodyBinary?: BinaryMeta;
    readonly headers?: SchemaDefinition;
    readonly returns?: SchemaDefinition;
    /** Raw octet success payload, mirroring `bodyBinary`. */
    readonly returnsBinary?: BinaryMeta;
  };
  /** Multi-status return and error response declarations for OpenAPI documentation. */
  responses?: ResponseDeclarations;
  /** Endpoint metadata for OpenAPI: security, cache, cors, rate-limit. */
  meta?: EndpointMeta;
  dependencies?: readonly string[];
  provenance?: { path: string; line?: number; symbol?: string };
}
