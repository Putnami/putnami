/**
 * A transport failure is an error the network call of a transport raised
 * itself — the connection, the write, the response head — as opposed to an
 * error some code on the way raised. Only the transports mark it, and the
 * mark keeps the error's own class, so existing classification (retry,
 * breaker) sees the same error it always did.
 */
const TRANSPORT_FAILURE = Symbol('putnami.client.transport-failure');

/** Mark an error raised by a transport's own network call, then return it. */
export function markTransportFailure(error: unknown): unknown {
  if (error instanceof Error && error.name !== 'AbortError' && error.name !== 'TimeoutError') {
    Object.defineProperty(error, TRANSPORT_FAILURE, { value: true, enumerable: false });
  }
  return error;
}

/** Whether a transport marked this error as its own network failure. */
export function isTransportFailure(error: unknown): boolean {
  return error instanceof Error && (error as { [TRANSPORT_FAILURE]?: boolean })[TRANSPORT_FAILURE] === true;
}
