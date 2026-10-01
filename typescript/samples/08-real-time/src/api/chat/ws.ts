import { endpoint } from '@putnami/application';
import { ArrayOf, Optional, Stream } from '@putnami/runtime';

interface OutMessage {
  type: string;
  username: string | undefined;
  text: string | undefined;
  users: string[] | undefined;
  timestamp: number;
}

const users = new Map<string, (msg: OutMessage) => void>();

function broadcast(msg: OutMessage) {
  for (const send of users.values()) {
    send(msg);
  }
}

// WebSocket chat room — messages are broadcast to all connected clients
export default endpoint()
  .body(
    Stream({
      type: String,
      username: Optional(String),
      text: Optional(String),
    }),
  )
  .returns(
    Stream({
      type: String,
      username: Optional(String),
      text: Optional(String),
      users: Optional(ArrayOf(String)),
      timestamp: Number,
    }),
  )
  .handle(async (ctx) => {
    let currentUser: string | undefined;

    for await (const message of ctx.messages()) {
      if (message.type === 'join' && message.username) {
        currentUser = message.username;
        users.set(currentUser, (msg) => ctx.send(msg));

        broadcast({ type: 'join', username: currentUser, text: undefined, users: undefined, timestamp: Date.now() });
        broadcast({
          type: 'users',
          username: undefined,
          text: undefined,
          users: [...users.keys()],
          timestamp: Date.now(),
        });
      } else if (message.type === 'message' && message.text) {
        broadcast({
          type: 'message',
          username: currentUser || 'Anonymous',
          text: message.text,
          users: undefined,
          timestamp: Date.now(),
        });
      }
    }

    // Client disconnected
    const left: OutMessage = {
      type: 'leave',
      username: currentUser,
      text: undefined,
      users: undefined,
      timestamp: Date.now(),
    };
    if (currentUser) {
      users.delete(currentUser);
      broadcast(left);
      broadcast({
        type: 'users',
        username: undefined,
        text: undefined,
        users: [...users.keys()],
        timestamp: Date.now(),
      });
    }

    // A bidirectional handler returns its terminal value. The raw WebSocket
    // bridge this sample uses discards it; a first-party client reads it as
    // the stream's result.
    return left;
  });
