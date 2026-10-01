import { createHash, randomUUID } from 'node:crypto';
import { type DetachedScope, useLogger } from '@putnami/runtime';
import type { HandlerDefinition } from '../handler/handler';
import type { Message } from '../topic/message';
import { createTransportMessage, withDrainTimeout, type Envelope, type Transport } from '../transport';
import {
  createBunRedisClient,
  toRedisCommandClient,
  type RedisCommandClient,
  type RedisCommandSource,
} from './bun-redis';
import { resolveRealtimeKey } from './realtime';
import {
  asError,
  DROP_RETRIES_EXHAUSTED,
  errorText,
  eventGroup,
  logDeadLettered,
  logDropped,
  logRetryScheduled,
} from '../transport/delivery-logging';
import { dispatchToHandler } from '../transport/dispatch';

/**
 * Transport-local logger name for failures that are NOT one of the contract's
 * event records (an undecodable record, a rejected XADD): dots like every other
 * framework logger name.
 */
const REDIS_STREAM_LOGGER = 'events.redis-stream';

export interface RedisStreamTransportConfig {
  /** Redis URL. Used only when client is not supplied. */
  url?: string;
  /** Redis command client. Must support XADD, XGROUP, XREADGROUP, and XACK. */
  client?: RedisCommandSource;
  /** Prefix for Redis stream keys. */
  keyPrefix?: string;
  /** Prefix for Redis consumer group names. */
  groupPrefix?: string;
  /** Consumer name. Defaults to a random process-local name. */
  consumerName?: string;
  /** Approximate maximum entries retained per stream. */
  maxLen?: number;
  /** XREADGROUP BLOCK timeout. Default: 5000ms. */
  blockMs?: number;
  /** Maximum entries read per XREADGROUP call. Default: 100. */
  count?: number;
  /** Close client created from url when stop() is called. */
  closeClient?: boolean;
  /**
   * Maximum time (ms) to wait for in-flight consume loops to settle during
   * `stop()`. After this deadline `stop()` proceeds (closing the client when
   * `closeClient` is set) even if a handler is still running. Default: 10_000.
   */
  drainTimeout?: number;
}

interface RedisSubscription {
  readonly definition: HandlerDefinition;
  readonly callback: (message: Message<unknown>) => Promise<void>;
  readonly group: string;
  pump?: Promise<void>;
}

interface RedisStreamRecord {
  id: string;
  message: string;
}

/**
 * Reliable Redis Streams transport using consumer groups.
 *
 * This is for background event handlers, not browser realtime fanout. It maps
 * successful handler returns to XACK and failed handlers to retry/DLQ streams.
 */
export class RedisStreamTransport implements Transport {
  private readonly client: RedisCommandClient;
  private readonly keyPrefix: string | undefined;
  private readonly groupPrefix: string | undefined;
  private readonly consumerName: string;
  private readonly maxLen: number | undefined;
  private readonly blockMs: number;
  private readonly count: number;
  private readonly closeClient: boolean;
  private readonly drainTimeout: number;
  private readonly subscriptions: RedisSubscription[] = [];
  private readonly retryTimers = new Set<ReturnType<typeof setTimeout>>();
  private running = false;
  private scopeFactory?: () => Promise<DetachedScope>;

  constructor(config: RedisStreamTransportConfig) {
    const source = config.client ?? (config.url ? createBunRedisClient(config.url) : undefined);
    if (!source) {
      throw new Error('Redis Stream transport requires a url or command client.');
    }

    this.client = toRedisCommandClient(source);
    this.keyPrefix = config.keyPrefix;
    this.groupPrefix = config.groupPrefix;
    this.consumerName = config.consumerName ?? `putnami-${randomUUID()}`;
    this.maxLen = config.maxLen;
    this.blockMs = config.blockMs ?? 5000;
    this.count = config.count ?? 100;
    this.closeClient = config.closeClient ?? Boolean(config.url);
    this.drainTimeout = Math.max(0, config.drainTimeout ?? 10_000);
  }

  setScopeFactory(factory: () => Promise<DetachedScope>): void {
    this.scopeFactory = factory;
  }

  async publish(_topic: string, envelope: Envelope): Promise<void> {
    await this.xadd(this.streamFor(envelope.topic), envelope);
  }

  async subscribe(
    definition: HandlerDefinition,
    callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    const subscription: RedisSubscription = {
      definition,
      callback,
      group: this.groupFor(definition),
    };
    this.subscriptions.push(subscription);
    if (this.running) {
      await this.ensureGroup(subscription);
      subscription.pump = this.consume(subscription);
    }
  }

  async start(): Promise<void> {
    this.running = true;
    await Promise.all(
      this.subscriptions.map(async (subscription) => {
        await this.ensureGroup(subscription);
        subscription.pump = this.consume(subscription);
      }),
    );
  }

  async stop(): Promise<void> {
    this.running = false;
    for (const timer of this.retryTimers) {
      clearTimeout(timer);
    }
    this.retryTimers.clear();
    // Bound the wait for in-flight consume loops so a stuck handler cannot hang
    // shutdown past the configured drain deadline.
    const pumps = Promise.allSettled(this.subscriptions.map((subscription) => subscription.pump));
    await withDrainTimeout(pumps, this.drainTimeout);
    if (this.closeClient) {
      await this.client.close?.();
    }
  }

  private async consume(subscription: RedisSubscription): Promise<void> {
    // Independent cursors for the shared topic stream and this group's own
    // retry stream. A retry re-published to the (topic, group) retry stream is
    // re-consumed only by THIS group's loop, never by the other broadcast
    // groups reading the shared topic stream.
    const cursors = { id: '0', retryId: '0' };
    const mainStream = this.streamFor(subscription.definition.topic.name);
    const retryStream = this.retryStreamFor(subscription.definition.topic.name, subscription.group);
    while (this.running) {
      // Topic stream is the primary source and blocks; the (usually idle) retry
      // stream is polled non-blocking (blockMs = 0). Blocking on the retry stream
      // would stall every iteration for the full blockMs before returning to the
      // topic, throttling topic throughput to Count events per blockMs window.
      await this.consumeStream(subscription, mainStream, cursors, 'id', this.blockMs);
      if (!this.running) {
        break;
      }
      await this.consumeStream(subscription, retryStream, cursors, 'retryId', 0);
    }
  }

  /**
   * Reads and dispatches one batch from a single stream using the named cursor.
   * Preserves the pending-drain cursor semantics: cursor '0' (or a concrete ID)
   * drains this group's PEL from that point, and an empty batch advances the
   * cursor to '>' for live delivery. A positive blockMs adds a BLOCK wait (topic
   * stream); blockMs <= 0 issues a non-blocking read (retry stream).
   */
  private async consumeStream(
    subscription: RedisSubscription,
    streamKey: string,
    cursors: { id: string; retryId: string },
    cursorKey: 'id' | 'retryId',
    blockMs: number,
  ): Promise<void> {
    const logger = useLogger(REDIS_STREAM_LOGGER);
    const args = ['GROUP', subscription.group, this.consumerName];
    if (blockMs > 0) {
      args.push('BLOCK', String(blockMs));
    }
    args.push('COUNT', String(this.count), 'STREAMS', streamKey, cursors[cursorKey]);
    const response = await this.client.command('XREADGROUP', args);

    const records = parseStreamResponse(response, streamKey);
    if (records.length === 0) {
      cursors[cursorKey] = '>';
      return;
    }

    const drainingPending = cursors[cursorKey] !== '>';
    for (const record of records) {
      try {
        await this.handleRecord(subscription, streamKey, record);
      } catch (error) {
        // Residual failure of the record pipeline itself (an XACK/XADD that
        // rejected, not the handler — handler failures are routed by
        // handleFailure). Stable message + structured fields + the real error.
        logger.error('stream record processing failed', asError(error), {
          event: { topic: subscription.definition.topic.name, streamKey, recordId: record.id },
        });
      }
    }
    if (drainingPending) {
      cursors[cursorKey] = records[records.length - 1]?.id ?? cursors[cursorKey];
    }
  }

  private async handleRecord(
    subscription: RedisSubscription,
    streamKey: string,
    record: RedisStreamRecord,
  ): Promise<void> {
    let envelope: Envelope;
    try {
      envelope = JSON.parse(record.message) as Envelope;
    } catch (error) {
      // An undecodable record is not retryable: it is acked (dropped). It carries
      // no envelope, so it cannot report the contract's `event` identity — it
      // stays a transport-local record rather than borrowing the closed drop
      // vocabulary with a shape no dashboard could join on.
      useLogger(REDIS_STREAM_LOGGER).error('stream record decode failed', asError(error), {
        event: { topic: subscription.definition.topic.name, streamKey, recordId: record.id },
      });
      await this.client.command('XACK', [streamKey, subscription.group, record.id]);
      return;
    }

    const opts = subscription.definition.options;

    try {
      const { message, ackState, abortController } = createTransportMessage(envelope, opts);
      await dispatchToHandler(
        message,
        ackState,
        abortController,
        envelope,
        subscription.definition,
        subscription.callback,
        {
          scopeFactory: this.scopeFactory,
        },
      );
      await this.client.command('XACK', [streamKey, subscription.group, record.id]);
    } catch (error) {
      await this.handleFailure(error, envelope, subscription, streamKey, record.id);
    }
  }

  private async handleFailure(
    error: unknown,
    envelope: Envelope,
    subscription: RedisSubscription,
    streamKey: string,
    recordId: string,
  ): Promise<void> {
    const opts = subscription.definition.options;

    if (envelope.attempt < opts.maxRetries) {
      const retryEnvelope: Envelope = { ...envelope, attempt: envelope.attempt + 1 };
      const delay = calculateBackoff(envelope.attempt, opts.maxBackoff);
      logRetryScheduled(envelope, delay, opts.maxRetries, error);
      const timer = setTimeout(() => {
        this.retryTimers.delete(timer);
        void this.retry(retryEnvelope, subscription, streamKey, recordId).catch((retryError) => {
          // The retry XADD failed, so the record stays pending in this group's PEL
          // and is redelivered — not dropped. Report it as a transport-local
          // failure rather than a drop the contract would have operators chase.
          useLogger(REDIS_STREAM_LOGGER).error('stream retry publish failed', asError(retryError), {
            event: { ...eventGroup(retryEnvelope), streamKey, recordId },
          });
        });
      }, delay);
      this.retryTimers.add(timer);
      return;
    }

    if (!opts.dlq) {
      // Retries are exhausted with no DLQ: the record is acked and the message is
      // lost. `attempt` is the true final delivery count, never attempt + 1.
      logDropped(envelope, DROP_RETRIES_EXHAUSTED, error);
      await this.client.command('XACK', [streamKey, subscription.group, recordId]);
      return;
    }

    const dlqEnvelope: Envelope = {
      ...envelope,
      topic: `${envelope.topic}.dlq`,
      attempt: 1,
      attributes: {
        ...envelope.attributes,
        'dlq.original_topic': envelope.topic,
        'dlq.original_attempt': String(envelope.attempt),
        'dlq.error': errorText(error),
      },
    };
    await this.xadd(this.streamFor(dlqEnvelope.topic), dlqEnvelope);
    // Emitted only once the dead-letter is actually written: a failed XADD leaves
    // the record pending in the group's PEL for redelivery, so claiming a
    // dead-letter there would be both premature and duplicated on the retry. The
    // group describes the ORIGINAL delivery (topic, true final attempt) plus the
    // dlqTopic it was routed to.
    logDeadLettered(envelope, dlqEnvelope.topic, error);
    await this.client.command('XACK', [streamKey, subscription.group, recordId]);
  }

  private async retry(
    retryEnvelope: Envelope,
    subscription: RedisSubscription,
    streamKey: string,
    recordId: string,
  ): Promise<void> {
    // Re-publish to THIS group's retry stream, not the shared topic stream.
    // XADDing back to streamFor(topic) would make the retry visible to every
    // other broadcast group's '>' cursor, re-running handlers that already
    // succeeded (cross-group fan-out amplification). The (topic, group)-keyed
    // retry stream is read only by this subscription's group loop.
    await this.xadd(this.retryStreamFor(retryEnvelope.topic, subscription.group), retryEnvelope);
    await this.client.command('XACK', [streamKey, subscription.group, recordId]);
  }

  private async ensureGroup(subscription: RedisSubscription): Promise<void> {
    await this.createGroup(this.streamFor(subscription.definition.topic.name), subscription.group);
    // The per-group retry stream needs its own consumer group so this loop can
    // XREADGROUP its retries.
    await this.createGroup(
      this.retryStreamFor(subscription.definition.topic.name, subscription.group),
      subscription.group,
    );
  }

  private async createGroup(streamKey: string, group: string): Promise<void> {
    try {
      await this.client.command('XGROUP', ['CREATE', streamKey, group, '$', 'MKSTREAM']);
    } catch (error) {
      if (!String(error).includes('BUSYGROUP')) {
        throw error;
      }
    }
  }

  private async xadd(streamKey: string, envelope: Envelope): Promise<void> {
    const args: string[] = [];
    if (this.maxLen && this.maxLen > 0) {
      args.push('MAXLEN', '~', String(this.maxLen));
    }
    args.push('*', 'message', JSON.stringify(envelope));
    await this.client.command('XADD', [streamKey, ...args]);
  }

  private streamFor(topic: string): string {
    return resolveRealtimeKey(this.keyPrefix, topic);
  }

  /**
   * Returns the retry stream key scoped to BOTH the topic and the consuming
   * group, so retries are isolated to the group that failed and never
   * redelivered to the other broadcast groups reading the shared topic stream.
   */
  private retryStreamFor(topic: string, group: string): string {
    return `${this.streamFor(topic)}:${group}:retry`;
  }

  private groupFor(definition: HandlerDefinition): string {
    if (definition.options.group) {
      return resolveRealtimeKey(this.groupPrefix, definition.options.group);
    }

    const topic = definition.topic.name.replaceAll('.', '-');
    if (definition.options.distribution === 'broadcast') {
      // Consumer-group names are durable Redis state. Deriving them from the
      // subscription's array position would silently rebind an existing
      // broadcast handler to a different group whenever handlers are added,
      // removed, or reordered — replaying old messages or skipping ahead on the
      // next deploy. Derive a stable identity from the handler itself instead so
      // the group survives reordering. Setting an explicit `group` is still the
      // recommended, fully-stable path for broadcast handlers.
      return resolveRealtimeKey(this.groupPrefix, `broadcast:${topic}:${broadcastIdentity(definition)}`);
    }
    return resolveRealtimeKey(this.groupPrefix, `competing:${topic}`);
  }
}

function parseStreamResponse(response: unknown, expectedStream: string): RedisStreamRecord[] {
  if (!Array.isArray(response)) {
    return [];
  }

  const records: RedisStreamRecord[] = [];
  for (const stream of response) {
    if (!Array.isArray(stream) || stream.length < 2) {
      continue;
    }
    if (String(stream[0]) !== expectedStream || !Array.isArray(stream[1])) {
      continue;
    }

    for (const entry of stream[1]) {
      if (!Array.isArray(entry) || entry.length < 2 || !Array.isArray(entry[1])) {
        continue;
      }

      const message = fieldValue(entry[1], 'message');
      if (message !== undefined) {
        records.push({ id: String(entry[0]), message });
      }
    }
  }
  return records;
}

function fieldValue(fields: unknown[], name: string): string | undefined {
  for (let i = 0; i < fields.length; i += 2) {
    if (String(fields[i]) === name) {
      return String(fields[i + 1]);
    }
  }
  return undefined;
}

function calculateBackoff(attempt: number, maxBackoff: number): number {
  const base = 1000;
  const delay = Math.min(base * 2 ** (attempt - 1), maxBackoff);
  return Math.floor(delay + delay * Math.random() * 0.25);
}

/**
 * Derive a stable identity for a broadcast handler that has no explicit
 * `group`. The identity is a short hash of the handler's topic, source text,
 * and attribute filter — deterministic across process restarts and unaffected
 * by subscription registration order. Two distinct handler functions on the
 * same topic get distinct groups (independent fan-out); the same handler keeps
 * its group across deploys as long as its body and filter are unchanged.
 */
function broadcastIdentity(definition: HandlerDefinition): string {
  const filterAttributes = definition.filter?.attributes ?? {};
  const filterKey = Object.keys(filterAttributes)
    .sort()
    .map((key) => `${key}=${filterAttributes[key]}`)
    .join('&');
  const source = `${definition.topic.name} ${String(definition.handler)} ${filterKey}`;
  return createHash('sha1').update(source).digest('hex').slice(0, 16);
}

export function redisStreamTransport(config: RedisStreamTransportConfig): RedisStreamTransport {
  return new RedisStreamTransport(config);
}
