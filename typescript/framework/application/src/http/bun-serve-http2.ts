/**
 * The `http2` option of `Bun.serve`, which the Bun runtime the workspace pins
 * accepts and the pinned `bun-types` release does not declare yet.
 *
 * Augmenting the declaration keeps the option typed where the server is
 * configured. Once `bun-types` declares it, this module is removed.
 */
declare module 'bun' {
  namespace Serve {
    interface BaseServeOptions<WebSocketData> {
      /**
       * Serve HTTP/2 in cleartext to a client that opens with the HTTP/2
       * preface, beside HTTP/1.1 on the same port.
       */
      http2?: boolean;
    }
  }
}

export {};
