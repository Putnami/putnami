/**
 * Headers that only an HTTP/1.x request carries.
 *
 * HTTP/2 forbids the connection-specific fields (RFC 9113 §8.2.2), and an
 * HTTP/1.1 request frames a body only with `Content-Length` or
 * `Transfer-Encoding` (RFC 9112 §6). An HTTP/2 request frames its body with the
 * stream itself, so a client that streams its messages sends neither.
 */
const HTTP1_FRAMING_HEADERS = [
  'connection',
  'keep-alive',
  'proxy-connection',
  'transfer-encoding',
  'upgrade',
  'content-length',
] as const;

/**
 * Whether a request arrived over HTTP/2 with a body the server reads as it
 * arrives.
 *
 * `Bun.serve` hands HTTP/1.1 and HTTP/2 requests to the same `fetch` handler
 * and states the protocol on neither, so the answer is read from the headers:
 * a request that carries a field HTTP/2 forbids, or the length HTTP/1.1 frames
 * a body with, is not one. A cleartext HTTP/2 caller that streams its body —
 * the only one a client or bidirectional stream can come from — sends none of
 * them. An HTTP/2 caller that states a `Content-Length` is read as HTTP/1.1,
 * and its client or bidirectional stream is refused UNIMPLEMENTED. An HTTP/1.1
 * request with no body sends none of them either, so a request whose body is
 * absent is not one: an HTTP/2 request that opens a body has one, even empty.
 */
export function arrivedOverHttp2(req: Request): boolean {
  // An HTTP/2 request that opens a body has one, even an empty one; an
  // HTTP/1.1 request that frames none has none.
  if (req.body === null) return false;
  for (const name of HTTP1_FRAMING_HEADERS) {
    if (req.headers.has(name)) return false;
  }
  return true;
}
