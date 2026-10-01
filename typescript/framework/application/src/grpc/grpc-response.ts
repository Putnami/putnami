import type { ProtoFieldMeta } from '../proto';
import { HttpResponse } from '../http/http-response';
import { encodeProto, type ProtoEnumRegistry } from './proto-encode';
import { CT_JSON, CT_PROTO, buildGrpcWebResponse, connectResponseHeaders } from './grpc-protocol';

const textEncoder = new TextEncoder();

// ---------------------------------------------------------------------------
// Response encoding
// ---------------------------------------------------------------------------

export async function formatGrpcResponse(
  result: HttpResponse,
  useBinary: boolean,
  responseMeta: ProtoFieldMeta[] | undefined,
  allMessages: Record<string, ProtoFieldMeta[]>,
  enums?: ProtoEnumRegistry,
  isGrpcWeb = false,
): Promise<HttpResponse> {
  // Fast path: use raw data directly (avoids JSON stringify → parse round-trip)
  const raw = result.rawData();
  if (raw !== undefined && typeof raw === 'object') {
    return encodeResponse(raw as Record<string, unknown>, useBinary, responseMeta, allMessages, enums, isGrpcWeb);
  }

  // Fallback: parse from serialized response body (non-JSON responses, streams, etc.)
  const response = result.get();
  const responseBody = await response.text();
  let data: unknown;
  try {
    data = JSON.parse(responseBody);
  } catch {
    data = undefined;
  }

  if (useBinary && responseMeta && data && typeof data === 'object') {
    const encoded = encodeProto(
      data as Record<string, unknown>,
      responseMeta,
      allMessages,
      enums?.types,
      enums?.values,
    );
    if (isGrpcWeb) {
      return buildGrpcWebResponse(encoded, true);
    }
    return new HttpResponse(encoded.buffer.slice(0) as ArrayBuffer, {
      status: response.status,
      headers: {
        'Content-Type': CT_PROTO,
        ...connectResponseHeaders(),
      },
    });
  }

  if (isGrpcWeb) {
    const payload = textEncoder.encode(responseBody || '{}');
    return buildGrpcWebResponse(payload, false);
  }

  // JSON path: forward the already-serialized body (no re-serialization)
  return new HttpResponse(responseBody, {
    status: response.status,
    headers: {
      'Content-Type': CT_JSON,
      ...connectResponseHeaders(),
    },
  });
}

export function encodeResponse(
  data: Record<string, unknown>,
  useBinary: boolean,
  responseMeta: ProtoFieldMeta[] | undefined,
  allMessages: Record<string, ProtoFieldMeta[]>,
  enums?: ProtoEnumRegistry,
  isGrpcWeb = false,
): HttpResponse {
  if (useBinary && responseMeta) {
    const encoded = encodeProto(data, responseMeta, allMessages, enums?.types, enums?.values);
    if (isGrpcWeb) {
      return buildGrpcWebResponse(encoded, true);
    }
    return new HttpResponse(encoded.buffer.slice(0) as ArrayBuffer, {
      headers: {
        'Content-Type': CT_PROTO,
        ...connectResponseHeaders(),
      },
    });
  }
  if (isGrpcWeb) {
    const payload = textEncoder.encode(JSON.stringify(data));
    return buildGrpcWebResponse(payload, false);
  }
  return HttpResponse.json(data, { headers: connectResponseHeaders() });
}

/** Format a handler result into a gRPC response. */
export function formatResult(
  result: unknown,
  useBinaryResponse: boolean,
  responseMeta: ProtoFieldMeta[] | undefined,
  messageMeta: Record<string, ProtoFieldMeta[]>,
  enums?: ProtoEnumRegistry,
  isGrpcWeb = false,
): HttpResponse | Promise<HttpResponse> {
  if (result instanceof HttpResponse) {
    return formatGrpcResponse(result, useBinaryResponse, responseMeta, messageMeta, enums, isGrpcWeb);
  }
  if (result === undefined || result === null) {
    return isGrpcWeb
      ? buildGrpcWebResponse(new Uint8Array(0), useBinaryResponse)
      : HttpResponse.json({}, { headers: connectResponseHeaders() });
  }
  if (typeof result === 'object') {
    return encodeResponse(
      result as Record<string, unknown>,
      useBinaryResponse,
      responseMeta,
      messageMeta,
      enums,
      isGrpcWeb,
    );
  }
  return HttpResponse.json(result, { headers: connectResponseHeaders() });
}
