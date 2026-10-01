/**
 * Observer for streaming responses from server-streaming or bidi RPCs.
 *
 * Server streams ride Connect envelope frames when the client speaks Connect,
 * and fall back to WebSocket otherwise; client/bidi streams always use
 * WebSocket. The observer shape is the same either way.
 *
 * @example
 * ```typescript
 * const stream = client.watchOrders(body);
 * stream.onMessage((order) => console.log('New order:', order));
 * stream.onError((err) => console.error('Stream error:', err));
 * stream.onComplete(() => console.log('Stream ended'));
 * stream.cancel(); // Clean up
 * ```
 */
export interface StreamObserver<T> {
  /** Register a handler for each incoming message */
  onMessage(handler: (data: T) => void): void;
  /** Register an error handler */
  onError(handler: (error: Error) => void): void;
  /** Register a completion handler */
  onComplete(handler: () => void): void;
  /** Cancel the stream and close the connection */
  cancel(): void;
}

/**
 * Duplex stream for client-streaming and bidi-streaming RPCs.
 */
export interface DuplexStream<TIn, TOut> extends StreamObserver<TOut> {
  /** Send a message to the server */
  send(data: TIn): void;
  /** Signal that no more messages will be sent */
  end(): void;
}
