export interface RedisCommandClient {
  command(command: string, args: string[]): Promise<unknown>;
  close?(): void | Promise<void>;
}

export interface RedisSendClient {
  send(command: string, args: string[]): Promise<unknown>;
  close?(): void | Promise<void>;
}

export interface RedisPubSubClient {
  publish(channel: string, message: string): Promise<unknown> | unknown;
  subscribe(channel: string, handler: (message: string, channel: string) => void): Promise<unknown> | unknown;
  unsubscribe?(channel: string, handler?: (message: string, channel: string) => void): Promise<unknown> | unknown;
  duplicate?(): RedisPubSubClient;
  close?(): void | Promise<void>;
}

export type RedisCommandSource = RedisCommandClient | RedisSendClient;

export function toRedisCommandClient(client: RedisCommandSource): RedisCommandClient {
  if (isRedisCommandClient(client)) {
    return client;
  }

  return {
    command: (command, args) => client.send(command, args),
    close: () => client.close?.(),
  };
}

export function createBunRedisClient(url: string): RedisSendClient & RedisPubSubClient {
  return new Bun.RedisClient(url) as unknown as RedisSendClient & RedisPubSubClient;
}

function isRedisCommandClient(client: RedisCommandSource): client is RedisCommandClient {
  return 'command' in client && typeof client.command === 'function';
}
