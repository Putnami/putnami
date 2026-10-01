import { validateSchema, type InferSchema, type SchemaDefinition } from '@putnami/runtime';
import { EVENT_FRAME_TYPES, PUTNAMI_EVENTS_PROTOCOL, type EventAuthFrame, type EventSubscribeFrame } from '../protocol';
import type { TopicDefinition } from '../topic/topic';
import {
  addWsListener,
  type EventServerControlFrame,
  isControlFrame,
  isEventFrame,
  parseFrame,
  removeWsListener,
  resolveMaybe,
  resolveReconnectPolicy,
  toWebSocketUrl,
} from './event-client.internal';

export interface EventClientConfig {
  /** WebSocket endpoint URL. http(s) URLs are converted to ws(s). */
  url: string;
  /** Bearer token or token resolver. Sent as the first frame after open. */
  token?: string | (() => string | Promise<string | undefined>) | undefined;
  /** Additional auth/routing headers. Sent as the first frame after open. */
  headers?: Record<string, string> | (() => Record<string, string> | Promise<Record<string, string> | undefined>);
  /** WebSocket implementation. Defaults to globalThis.WebSocket. */
  webSocket?: WebSocketConstructor;
  /** Connection timeout in milliseconds. Default: 15000. */
  connectTimeoutMs?: number;
  /** Reconnect policy. Disabled by default. */
  reconnect?: boolean | EventClientReconnectOptions;
}

export interface EventClientReconnectOptions {
  /** Maximum reconnect attempts. Default: Infinity. */
  retries?: number;
  /** Initial reconnect delay. Default: 500ms. */
  minDelayMs?: number;
  /** Maximum reconnect delay. Default: 10000ms. */
  maxDelayMs?: number;
}

export interface EventClientSubscribeOptions {
  /** Initial replay cursor. Pass a previous message id for resume semantics. */
  from?: 'latest' | 'earliest' | string;
  /** Optional attributes forwarded to the gateway subscribe frame. */
  attributes?: Record<string, string>;
  /** Abort signal that closes the subscription. */
  signal?: AbortSignal;
  /** Called when the WebSocket opens or reconnects. */
  onOpen?: () => void;
  /** Called when the WebSocket closes. */
  onClose?: (event: EventClientCloseEvent) => void;
  /** Called for protocol, validation, parse, and transport errors. */
  onError?: (error: Error) => void;
}

export interface EventClientCloseEvent {
  code?: number;
  reason?: string;
  wasClean?: boolean;
}

export interface ClientEventMessage<T = unknown> {
  readonly id: string;
  readonly topic: string;
  readonly channel?: string;
  readonly payload: T;
  readonly key?: string;
  readonly dedupeKey?: string;
  readonly topicVersion?: string;
  readonly timestamp: Date;
  readonly attributes: Readonly<Record<string, string>>;
  readonly traceId?: string;
}

export interface EventSubscription {
  /** Resolves after the first WebSocket connection opens and subscribes. */
  readonly ready: Promise<void>;
  /** Close the subscription and stop reconnect attempts. */
  close(code?: number, reason?: string): void;
}

interface ManagedEventSubscription extends EventSubscription {
  readonly closed: boolean;
  open(): void;
}

export interface EventClient {
  subscribe<S extends SchemaDefinition>(
    topic: TopicDefinition<S>,
    handler: (message: ClientEventMessage<InferSchema<S>>) => void | Promise<void>,
    options?: EventClientSubscribeOptions,
  ): EventSubscription;
  close(): void;
}

export type EventClientSubscribeFrame = EventSubscribeFrame;
export type EventClientAuthFrame = EventAuthFrame;

type WebSocketConstructor = new (url: string) => WebSocket;

const DEFAULT_CONNECT_TIMEOUT_MS = 15_000;

export function eventClient(config: EventClientConfig): EventClient {
  return new WebSocketEventClient(config);
}

class WebSocketEventClient implements EventClient {
  private readonly subscriptions = new Set<ManagedEventSubscription>();

  constructor(private readonly config: EventClientConfig) {}

  subscribe<S extends SchemaDefinition>(
    topic: TopicDefinition<S>,
    handler: (message: ClientEventMessage<InferSchema<S>>) => void | Promise<void>,
    options: EventClientSubscribeOptions = {},
  ): EventSubscription {
    const subscription = new WebSocketEventSubscription(this.config, topic, handler, options);
    this.subscriptions.add(subscription);
    subscription.ready
      .catch(() => undefined)
      .then(() => {
        if (subscription.closed) {
          this.subscriptions.delete(subscription);
        }
      });
    subscription.open();
    return subscription;
  }

  close(): void {
    for (const subscription of this.subscriptions) {
      subscription.close();
    }
    this.subscriptions.clear();
  }
}

class WebSocketEventSubscription<S extends SchemaDefinition> implements ManagedEventSubscription {
  readonly ready: Promise<void>;
  private resolveReady!: () => void;
  private rejectReady!: (error: Error) => void;
  private ws: WebSocket | undefined;
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  private connectTimeout: ReturnType<typeof setTimeout> | undefined;
  private reconnectAttempt = 0;
  private connectionGeneration = 0;
  private readonly deliveryQueue: Array<{ message: ClientEventMessage<InferSchema<S>>; generation: number }> = [];
  private readonly blockedCursorGenerations = new Set<number>();
  private lastEventId: string | undefined;
  private manuallyClosed = false;
  private delivering = false;
  closed = false;

  constructor(
    private readonly config: EventClientConfig,
    private readonly topic: TopicDefinition<S>,
    private readonly handler: (message: ClientEventMessage<InferSchema<S>>) => void | Promise<void>,
    private readonly options: EventClientSubscribeOptions,
  ) {
    this.lastEventId =
      options.from && options.from !== 'latest' && options.from !== 'earliest' ? options.from : undefined;
    this.ready = new Promise<void>((resolve, reject) => {
      this.resolveReady = resolve;
      this.rejectReady = reject;
    });
    options.signal?.addEventListener('abort', () => this.close(), { once: true });
  }

  open(): void {
    if (this.manuallyClosed) {
      return;
    }

    const WebSocketImpl = this.config.webSocket ?? globalThis.WebSocket;
    if (!WebSocketImpl) {
      this.fail(new Error('WebSocket is not available in this environment.'));
      return;
    }

    const ws = new WebSocketImpl(toWebSocketUrl(this.config.url));
    this.ws = ws;
    const generation = ++this.connectionGeneration;

    this.connectTimeout = setTimeout(() => {
      ws.close();
      this.fail(
        new Error(`WebSocket connection timeout after ${this.config.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS}ms`),
      );
    }, this.config.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS);

    const cleanup = () => {
      clearTimeout(this.connectTimeout);
      removeWsListener(ws, 'open', onOpen);
      removeWsListener(ws, 'message', onMessage);
      removeWsListener(ws, 'error', onError);
      removeWsListener(ws, 'close', onClose);
    };

    const onOpen = () => {
      clearTimeout(this.connectTimeout);
      this.reconnectAttempt = 0;
      this.sendHandshake(ws).then(
        () => {
          this.resolveReady();
          this.options.onOpen?.();
        },
        (error) => this.fail(error instanceof Error ? error : new Error(String(error))),
      );
    };

    const onMessage = (event: MessageEvent) => {
      this.handleRawMessage(event.data, generation);
    };

    const onError = (event: Event) => {
      const message = event instanceof ErrorEvent && event.message ? event.message : 'unknown WebSocket error';
      this.options.onError?.(new Error(`WebSocket error: ${message}`));
    };

    const onClose = (event: CloseEvent) => {
      cleanup();
      this.options.onClose?.({ code: event.code, reason: event.reason, wasClean: event.wasClean });
      if (this.manuallyClosed) {
        this.closed = true;
        return;
      }
      this.scheduleReconnect();
    };

    addWsListener(ws, 'open', onOpen);
    addWsListener(ws, 'message', onMessage);
    addWsListener(ws, 'error', onError);
    addWsListener(ws, 'close', onClose);
  }

  close(code?: number, reason?: string): void {
    this.manuallyClosed = true;
    this.closed = true;
    clearTimeout(this.reconnectTimer);
    clearTimeout(this.connectTimeout);
    this.ws?.close(code, reason);
  }

  private async sendHandshake(ws: WebSocket): Promise<void> {
    const [token, headers] = await Promise.all([resolveMaybe(this.config.token), resolveMaybe(this.config.headers)]);
    if (token || headers) {
      ws.send(
        JSON.stringify({
          protocol: PUTNAMI_EVENTS_PROTOCOL,
          type: EVENT_FRAME_TYPES.auth,
          token,
          headers,
        } satisfies EventClientAuthFrame),
      );
    }

    const from = this.lastEventId ?? this.options.from;
    ws.send(
      JSON.stringify({
        protocol: PUTNAMI_EVENTS_PROTOCOL,
        type: EVENT_FRAME_TYPES.subscribe,
        topic: this.topic.name,
        channel: this.topic.channel,
        from,
        attributes: this.options.attributes,
      } satisfies EventClientSubscribeFrame),
    );
  }

  private handleRawMessage(raw: unknown, generation: number): void {
    try {
      const frame = parseFrame(raw);
      if (isControlFrame(frame)) {
        this.handleControlFrame(frame);
        return;
      }
      if (!isEventFrame(frame)) {
        throw new Error('Invalid Event Server frame.');
      }
      if (frame.topic !== this.topic.name) {
        return;
      }

      const { errors } = validateSchema(this.topic.schema, frame.payload, { label: this.topic.name });
      if (errors.length > 0) {
        const messages = errors.map((error) => `${error.field}: ${error.message}`).join('; ');
        throw new Error(`Invalid payload for topic '${this.topic.name}': ${messages}`);
      }

      const message: ClientEventMessage<InferSchema<S>> = {
        id: frame.id,
        topic: frame.topic,
        channel: frame.channel,
        payload: frame.payload as InferSchema<S>,
        key: frame.key,
        dedupeKey: frame.dedupeKey,
        topicVersion: frame.topicVersion,
        timestamp: frame.timestamp ? new Date(frame.timestamp) : new Date(),
        attributes: frame.attributes ?? {},
        traceId: frame.traceId,
      };

      this.deliveryQueue.push({ message, generation });
      this.drainDeliveryQueue();
    } catch (error) {
      this.options.onError?.(error instanceof Error ? error : new Error(String(error)));
    }
  }

  private drainDeliveryQueue(): void {
    if (this.delivering) {
      return;
    }
    this.delivering = true;
    this.runDeliveryQueue().catch((error) => {
      this.delivering = false;
      this.options.onError?.(error instanceof Error ? error : new Error(String(error)));
    });
  }

  private runDeliveryQueue(): Promise<void> {
    const delivery = this.deliveryQueue.shift();
    if (!delivery) {
      this.delivering = false;
      return Promise.resolve();
    }
    return this.deliverMessage(delivery.message, delivery.generation).then(() => this.runDeliveryQueue());
  }

  private async deliverMessage(message: ClientEventMessage<InferSchema<S>>, generation: number): Promise<void> {
    try {
      await this.handler(message);
      if (!this.blockedCursorGenerations.has(generation)) {
        this.lastEventId = message.id;
      }
    } catch (error) {
      this.blockedCursorGenerations.add(generation);
      this.options.onError?.(error instanceof Error ? error : new Error(String(error)));
    }
  }

  private handleControlFrame(frame: EventServerControlFrame): void {
    if (frame.type === 'error') {
      this.options.onError?.(new Error(frame.message ?? frame.error ?? 'Event Server error'));
      return;
    }
    this.close();
  }

  private scheduleReconnect(): void {
    const policy = resolveReconnectPolicy(this.config.reconnect);
    if (!policy) {
      this.closed = true;
      return;
    }

    if (this.reconnectAttempt >= policy.retries) {
      this.closed = true;
      this.options.onError?.(new Error('Event subscription reconnect attempts exhausted.'));
      return;
    }

    const delay = Math.min(policy.minDelayMs * 2 ** this.reconnectAttempt, policy.maxDelayMs);
    this.reconnectAttempt++;
    this.reconnectTimer = setTimeout(() => this.open(), delay);
  }

  private fail(error: Error): void {
    this.rejectReady(error);
    this.options.onError?.(error);
  }
}
