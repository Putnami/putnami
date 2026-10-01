import { HttpException } from '@putnami/runtime';
import { getHttpStatusText } from '../../http/http-status.type';
import { IMPLICIT_BAD_REQUEST_CODE, IMPLICIT_INTERNAL_CODE, stableWireCode } from '../route/error-codes';
import type { ErrorResponseCode } from '../route/response-meta';
import type { StreamEndpointDefinition } from '../route/stream-endpoint';
import { validateSchema } from '../route/validate';

/**
 * The D0.1 first-party error envelope, shared by SSE terminal events and the
 * WebSocket `error` frame.
 *
 * It is the same shape `firstPartyErrorBody` writes for a unary call, plus the
 * `status` a stream has no HTTP response line to carry it in. One envelope
 * means a consumer decodes a declared error the same way whichever transport
 * delivered it. The WebSocket `error` frame drops `error` — the wire's closed
 * field set has no status phrase — and carries the rest unchanged.
 */
export interface FirstPartyStreamError {
  readonly status: number;
  readonly code: string;
  readonly error: string;
  readonly message: string;
  readonly details?: unknown;
}

/**
 * Project a thrown value into the stable, sanitized terminal envelope.
 *
 * `code` is the stable wire code (`not_found`, `http.internal_server`), never
 * the PascalCase `.mayThrow()` identifier: a consumer narrows the terminal
 * against the contract it was generated from, so a code that only the provider
 * recognizes is a broken typed terminal.
 *
 * An error the endpoint never declared is never presented as a declared one. It
 * falls back to the implicit pair by status, or to `client.remote`, and carries
 * no details: the consumer's typed decoder has nothing to validate them
 * against, and forwarding an undeclared body through a declared error's slot is
 * exactly the leak this projection exists to prevent.
 */
export function projectStreamError(error: unknown, def: StreamEndpointDefinition): FirstPartyStreamError {
  if (!(error instanceof HttpException)) {
    return {
      status: 500,
      code: IMPLICIT_INTERNAL_CODE,
      error: getHttpStatusText(500) ?? 'Internal Server Error',
      message: 'Internal Server Error',
    };
  }
  const status = error.getStatus();
  const runtimeCode = error.code.endsWith('Exception') ? error.code.slice(0, -'Exception'.length) : error.code;
  const declared = def.responses?.errorCodes?.includes(runtimeCode as never) ?? false;
  const code = declared
    ? stableWireCode(error.code, status)
    : status === 400
      ? IMPLICIT_BAD_REQUEST_CODE
      : status === 500
        ? IMPLICIT_INTERNAL_CODE
        : 'client.remote';
  const envelope = {
    status,
    code,
    error: getHttpStatusText(status) ?? error.name,
    message: error.message,
  } satisfies FirstPartyStreamError;
  const declaredDetail =
    code !== 'client.remote' && code !== IMPLICIT_BAD_REQUEST_CODE && code !== IMPLICIT_INTERNAL_CODE;
  // `.mayThrowDetails()` names the schema for this code; the older
  // `.throws(status, …, schema)` form names it for the status.
  const detailSchema = declaredDetail
    ? (def.responses?.errorDetails?.[runtimeCode as ErrorResponseCode] ??
      def.responses?.throws?.find((entry) => entry.status === status)?.schema)
    : undefined;
  if (!detailSchema) return envelope;
  try {
    return { ...envelope, details: validateSchema(detailSchema, error.getResponse(), { label: 'error details' }) };
  } catch {
    return envelope;
  }
}
