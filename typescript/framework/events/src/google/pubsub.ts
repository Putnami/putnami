import { randomUUID } from 'node:crypto';
import { type DetachedScope, useLogger } from '@putnami/runtime';
import { asError } from '../transport/delivery-logging';
import { dispatchToHandler } from '../transport/dispatch';
import type { HandlerDefinition } from '../handler/handler';
import type { Message } from '../topic/message';
import { createTransportMessage, type Envelope, type Transport } from '../transport';

/**
 * Transport-local logger name for failures that are NOT one of the contract's
 * event records (an undecodable message, a streaming-pull error): dots like
 * every other framework logger name.
 */
const GOOGLE_PUBSUB_LOGGER = 'events.google-pubsub';

export interface GooglePubSubTopic {
  publishMessage(message: {
    data: Uint8Array;
    attributes?: Record<string, string>;
    orderingKey?: string;
  }): Promise<unknown> | unknown;
}

export interface GooglePubSubSubscription {
  on(event: 'message', handler: (message: GooglePubSubMessage) => void): unknown;
  on(event: 'error', handler: (error: unknown) => void): unknown;
  removeListener?(event: 'message', handler: (message: GooglePubSubMessage) => void): unknown;
  removeListener?(event: 'error', handler: (error: unknown) => void): unknown;
  close?(): Promise<void> | void;
}

export interface GooglePubSubMessage {
  readonly id?: string;
  readonly data: Uint8Array | string;
  readonly attributes?: Record<string, string>;
  ack(): void;
  nack(): void;
}

export interface GooglePubSubClient {
  topic(name: string): GooglePubSubTopic;
  subscription(name: string): GooglePubSubSubscription;
}

export interface GooglePubSubTransportConfig {
  client: GooglePubSubClient;
  /** Maps Putnami topic names to Google Pub/Sub topic names. */
  topicName?: (topic: string) => string;
  /** Maps handler definitions to subscription names. Defaults to handler group or topic. */
  subscriptionName?: (definition: HandlerDefinition) => string;
  /** Called when the underlying Pub/Sub streaming pull emits an error. */
  onError?: (error: unknown, context: GooglePubSubErrorContext) => void | Promise<void>;
  /** Close subscriptions on stop. Default: true. */
  closeSubscriptions?: boolean;
}

export interface GooglePubSubErrorContext {
  readonly definition: HandlerDefinition;
  readonly subscription: GooglePubSubSubscription;
  readonly subscriptionName: string;
}

interface RegisteredSubscription {
  readonly definition: HandlerDefinition;
  readonly callback: (message: Message<unknown>) => Promise<void>;
  readonly subscription: GooglePubSubSubscription;
  readonly subscriptionName: string;
  readonly onMessage: (message: GooglePubSubMessage) => void;
  readonly onError: (error: unknown) => void;
}

/**
 * Google Pub/Sub transport for reliable event handlers.
 *
 * Retry and DLQ timing are delegated to the Pub/Sub subscription
 * configuration. This transport maps handler success to ack and failure to
 * nack, preserving Putnami handler timeout and manual ack semantics locally.
 */
export class GooglePubSubTransport implements Transport {
  private readonly client: GooglePubSubClient;
  private readonly topicName: (topic: string) => string;
  private readonly subscriptionName: (definition: HandlerDefinition) => string;
  private readonly onError: GooglePubSubTransportConfig['onError'];
  private readonly closeSubscriptions: boolean;
  private readonly subscriptions: RegisteredSubscription[] = [];
  private readonly inFlight = new Set<Promise<void>>();
  private scopeFactory?: () => Promise<DetachedScope>;

  constructor(config: GooglePubSubTransportConfig) {
    this.client = config.client;
    this.topicName = config.topicName ?? ((topic) => topic);
    this.subscriptionName =
      config.subscriptionName ??
      ((definition) => definition.options.group ?? definition.topic.name.replaceAll('.', '-'));
    this.onError = config.onError;
    this.closeSubscriptions = config.closeSubscriptions ?? true;
  }

  setScopeFactory(factory: () => Promise<DetachedScope>): void {
    this.scopeFactory = factory;
  }

  async publish(topic: string, envelope: Envelope): Promise<void> {
    await this.client.topic(this.topicName(topic)).publishMessage({
      data: new TextEncoder().encode(JSON.stringify(envelope)),
      attributes: envelope.attributes,
      orderingKey: envelope.key,
    });
  }

  async subscribe(
    definition: HandlerDefinition,
    callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    const subscriptionName = this.subscriptionName(definition);
    const subscription = this.client.subscription(subscriptionName);
    const onMessage = (message: GooglePubSubMessage) => {
      const delivery = this.handleMessage(definition, callback, message).finally(() => {
        this.inFlight.delete(delivery);
      });
      this.inFlight.add(delivery);
    };
    const onError = (error: unknown) => {
      void this.handleSubscriptionError(error, { definition, subscription, subscriptionName });
    };
    this.subscriptions.push({ definition, callback, subscription, subscriptionName, onMessage, onError });
  }

  async start(): Promise<void> {
    for (const subscription of this.subscriptions) {
      subscription.subscription.on('message', subscription.onMessage);
      subscription.subscription.on('error', subscription.onError);
    }
  }

  async stop(): Promise<void> {
    for (const subscription of this.subscriptions) {
      subscription.subscription.removeListener?.('message', subscription.onMessage);
      subscription.subscription.removeListener?.('error', subscription.onError);
      if (this.closeSubscriptions) {
        await subscription.subscription.close?.();
      }
    }
    // Drain handler invocations already dispatched so a stop() caller
    // observes every delivery acked/nacked, not abandoned mid-flight.
    await Promise.allSettled([...this.inFlight]);
  }

  private async handleMessage(
    definition: HandlerDefinition,
    callback: (message: Message<unknown>) => Promise<void>,
    raw: GooglePubSubMessage,
  ): Promise<void> {
    // Decode before the protective dispatch try/catch can throw on a non-JSON /
    // truncated body. Ack (drop) the poison message rather than letting it
    // redeliver forever — a parse failure is not retryable, and leaving the
    // delivery un-acked/un-nacked stalls the subscription. Mirrors the Redis
    // Stream transport, which XACKs an undecodable record.
    let envelope: Envelope;
    try {
      envelope = decodeEnvelope(raw);
    } catch (error) {
      // Undecodable (poison) message: acked, so it is lost. It has no envelope,
      // so it cannot report the contract's `event` identity — it stays a
      // transport-local record instead of borrowing the closed drop vocabulary
      // with a shape no dashboard could join on.
      useLogger(GOOGLE_PUBSUB_LOGGER).error('pubsub message decode failed', asError(error), {
        event: { topic: definition.topic.name, ...(raw.id ? { messageId: raw.id } : {}) },
      });
      raw.ack();
      return;
    }

    const opts = definition.options;
    const { message, ackState, abortController } = createTransportMessage(envelope, opts);

    try {
      await dispatchToHandler(message, ackState, abortController, envelope, definition, callback, {
        scopeFactory: this.scopeFactory,
      });
      raw.ack();
    } catch {
      raw.nack();
    }
  }

  private async handleSubscriptionError(error: unknown, context: GooglePubSubErrorContext): Promise<void> {
    const logger = useLogger(GOOGLE_PUBSUB_LOGGER);
    // Subscription-level (not delivery-level) failures: stable messages, the
    // subscription identity as structured fields, and the real error object.
    const fields = {
      event: { topic: context.definition.topic.name, subscription: context.subscriptionName },
    };
    logger.error('pubsub subscription error', asError(error), fields);

    try {
      await this.onError?.(error, context);
    } catch (callbackError) {
      logger.error('pubsub subscription error callback failed', asError(callbackError), fields);
    }
  }
}

function decodeEnvelope(message: GooglePubSubMessage): Envelope {
  const raw = typeof message.data === 'string' ? message.data : new TextDecoder().decode(message.data);
  const envelope = JSON.parse(raw) as Envelope;
  return {
    ...envelope,
    id: envelope.id || message.id || randomUUID(),
    attributes: envelope.attributes ?? message.attributes ?? {},
  };
}

export function googlePubSubTransport(config: GooglePubSubTransportConfig): GooglePubSubTransport {
  return new GooglePubSubTransport(config);
}
