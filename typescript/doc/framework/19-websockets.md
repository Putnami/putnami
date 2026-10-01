# WebSockets & Streaming

`@putnami/application` includes WebSocket and SSE support built on Bun's native WebSocket server. Create real-time applications with typed, validated streaming endpoints.

## Streaming with `Stream()`

Define real-time endpoints using the `Stream()` combinator with the `endpoint()` builder. Wrap a schema in `Stream()` to mark it as a stream of messages. The streaming mode is derived from which slots are streams:

| `body` | `returns` | Mode | Transport |
| ----------- | ----------- | ------------- | -------------------- |
| `T` | `T` | Unary | REST (standard HTTP) |
| `T` | `Stream(T)` | Server-stream | SSE or WebSocket |
| `Stream(T)` | `T` | Client-stream | WebSocket |
| `Stream(T)` | `Stream(T)` | Bidirectional | WebSocket |

### Server-stream

Push events to the client. Supports both SSE and WebSocket via protocol negotiation.

```ts
// src/api/events/ws.ts
import { endpoint, Stream } from '@putnami/application';

export default endpoint()
  .returns(Stream({ event: String, payload: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'welcome', payload: 'hello' });
    // handler runs until it returns, then the connection closes
  });
```

### Client-stream

Consume a stream of messages from the client, return a final result:

```ts
// src/api/upload/ws.ts
import { endpoint, Stream } from '@putnami/application';

export default endpoint()
  .body(Stream({ type: String, data: String }))
  .returns({ result: String })
  .handle(async (ctx) => {
    let count = 0;
    for await (const msg of ctx.messages()) {
      count++;
    }
    return { result: `processed ${count} messages` };
  });
```

### Bidirectional

Send and receive concurrently:

```ts
// src/api/chat/[roomId]/ws.ts
import { endpoint, Stream, Uuid } from '@putnami/application';

export default endpoint()
  .params({ roomId: Uuid })
  .body(Stream({ type: String, data: String }))
  .returns(Stream({ event: String, payload: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'welcome', payload: ctx.params.roomId });
    for await (const msg of ctx.messages()) {
      ctx.send({ event: 'echo', payload: msg.data });
    }
    return { event: 'bye', payload: ctx.params.roomId };
  });
```

The handler returns its terminal value, typed by the `returns` element schema. A first-party client reads it as the stream's result; the raw WebSocket bridge discards it.

### Handler context

The handler context adapts based on the streaming mode:

- `ctx.messages()` — available when body is `Stream()`, returns `AsyncIterable<T>`. The loop ends when the client disconnects.
- `ctx.send(data)` — available when returns is `Stream()`, pushes a validated message to the client.
- `ctx.params`, `ctx.queryParams()` — validated on connection open, same as HTTP endpoints.

Schema validation applies to all stream messages automatically. Invalid messages are rejected before reaching the handler.

### Protocol negotiation (server-stream)

Server-stream endpoints support two transports:

**WebSocket** — the client sends `Upgrade: websocket`:

```ts
const ws = new WebSocket('ws://localhost:3000/events');
ws.onmessage = (e) => console.log(JSON.parse(e.data));
```

**SSE** — the client sends `Accept: text/event-stream`:

```ts
const source = new EventSource('http://localhost:3000/events');
source.onmessage = (e) => console.log(JSON.parse(e.data));
```

Client-stream and bidirectional endpoints are WebSocket-only.

## Client-side connection

### Basic client

```ts
const ws = new WebSocket('ws://localhost:3000/api/chat/room');

ws.onopen = () => {
  console.log('Connected');
  ws.send(JSON.stringify({ type: 'hello' }));
};

ws.onmessage = (event) => {
  const data = JSON.parse(event.data);
  console.log('Received:', data);
};

ws.onclose = (event) => {
  console.log('Disconnected:', event.code, event.reason);
};

ws.onerror = (error) => {
  console.error('Error:', error);
};
```

### SSE client (server-stream)

```ts
const source = new EventSource('http://localhost:3000/api/events');

source.onmessage = (event) => {
  const data = JSON.parse(event.data);
  console.log('Event:', data);
};

source.onerror = () => {
  console.log('Connection lost — EventSource will auto-reconnect');
};
```

### React hook

```tsx
import { useEffect, useState, useCallback, useRef } from 'react';

function useWebSocket(url: string) {
  const [isConnected, setIsConnected] = useState(false);
  const [lastMessage, setLastMessage] = useState<unknown>(null);
  const wsRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    const ws = new WebSocket(url);
    wsRef.current = ws;

    ws.onopen = () => setIsConnected(true);
    ws.onclose = () => setIsConnected(false);
    ws.onmessage = (event) => {
      setLastMessage(JSON.parse(event.data));
    };

    return () => {
      ws.close();
    };
  }, [url]);

  const send = useCallback((data: unknown) => {
    wsRef.current?.send(JSON.stringify(data));
  }, []);

  return { isConnected, lastMessage, send };
}

// Usage
function ChatComponent() {
  const { isConnected, lastMessage, send } = useWebSocket('ws://localhost:3000/ws/chat');

  return (
    <div>
      <p>Status: {isConnected ? 'Connected' : 'Disconnected'}</p>
      <button onClick={() => send({ type: 'ping' })}>
        Send Ping
      </button>
    </div>
  );
}
```

### Reconnection logic

```ts
function createReconnectingWebSocket(url: string, options = { maxRetries: 5 }) {
  let ws: WebSocket | null = null;
  let retries = 0;

  const connect = () => {
    ws = new WebSocket(url);

    ws.onopen = () => {
      retries = 0;
      console.log('Connected');
    };

    ws.onclose = () => {
      if (retries < options.maxRetries) {
        retries++;
        const delay = Math.min(1000 * Math.pow(2, retries), 30000);
        console.log(`Reconnecting in ${delay}ms...`);
        setTimeout(connect, delay);
      }
    };
  };

  connect();

  return {
    send: (data: unknown) => ws?.send(JSON.stringify(data)),
    close: () => ws?.close(),
  };
}
```

## Best practices

1. **Use `Stream()` for typed endpoints** — schema validation catches issues early
2. **Prefer SSE for server-push** — simpler, works through proxies, auto-reconnects
3. **Handle errors gracefully** — always handle connection errors
4. **Authenticate connections** — verify user identity before allowing connections
5. **Limit message size** — prevent abuse with large messages
6. **Clean up resources** — the `for await` loop ends automatically on disconnect

## Related guides

- [API](/docs/frameworks/typescript/api)
- [HTTP & Middleware](/docs/frameworks/typescript/http-and-middleware)
