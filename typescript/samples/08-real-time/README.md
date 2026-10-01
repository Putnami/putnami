# Real-Time

WebSocket, SSE, and streaming.

## Features

- WebSocket handler in `ws.ts` using the streaming endpoint API: `endpoint().body(Stream(...)).returns(Stream(...))`, consuming inbound messages with `for await (const msg of ctx.messages())` and pushing replies with `ctx.send(...)`
- Server-Sent Events (SSE) for push notifications
- Broadcasting to multiple clients
- User presence tracking
- Client UI (HTML/JS) for testing

## Routes

| Type | Path | Description |
|------|------|-------------|
| WebSocket | `/chat` | Real-time chat room |
| SSE | `/notifications` | Push notification stream |
| Static | `/` | Client UI |

## Run

```bash
putnami serve .
```

## Try It

**1. Open the chat UI:**

Open [http://localhost:3908/](http://localhost:3908/) in your browser.

Expected result — a chat interface with a username prompt and message input.

**2. Open a second tab:**

Open [http://localhost:3908/](http://localhost:3908/) in another browser tab.

Expected result — two independent chat clients. Join with different usernames in each tab.

**3. Send messages:**

Type a message in one tab and send it.

Expected result — the message appears in both tabs in real-time. The sender sees their own message and the other tab receives the broadcast.

**4. Observe presence tracking:**

Expected result — when a user joins, all clients receive a user list update. When a tab is closed, a "leave" event is broadcast.

**5. Test SSE notifications:**

```bash
curl -N http://localhost:3908/notifications
```

Expected result — a `text/event-stream` response. Each `ctx.send(...)` emits a
default-event SSE frame (`data:` only, no `event:` line), so the browser reads
them via `EventSource.onmessage`. The handler sends an initial `Connected`
frame, then one notification every 3 seconds:

```
data: {"id":0,"message":"Connected","timestamp":1705312200000}

data: {"id":1,"message":"Notification #1","timestamp":1705312203000}

data: {"id":2,"message":"Notification #2","timestamp":1705312206000}
```

Press `Ctrl+C` to stop the stream.

## Test

```bash
putnami test .
```

## What this sample proves

The test starts the actual API and static plugins, loads the browser client,
opens a WebSocket, observes the typed join/user-list exchange, and checks that
the notification endpoint negotiates `text/event-stream`. The HTML/JavaScript
client is intentionally a static asset; this sample does not claim React SSR,
hydration, loaders, or actions.

WebSocket and SSE endpoint transport are owned by
[`@putnami/application`](../../framework/application/README.md), not
`@putnami/web`. The public [WebSockets and streaming
guide](/docs/frameworks/typescript/websockets) documents that owning API; no
parallel React/web feature is declared here.

## WebSocket Protocol

### Client to Server

```json
{ "type": "join", "username": "Alice" }
{ "type": "message", "text": "Hello!" }
```

### Server to Client

```json
{ "type": "join", "username": "Alice", "timestamp": 1234567890 }
{ "type": "leave", "username": "Bob", "timestamp": 1234567890 }
{ "type": "message", "username": "Alice", "text": "Hello!", "timestamp": 1234567890 }
{ "type": "users", "users": ["Alice", "Bob"] }
```
