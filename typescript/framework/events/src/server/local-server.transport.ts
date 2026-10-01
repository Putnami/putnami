import type { DetachedScope } from '@putnami/runtime';
import { PUTNAMI_EVENTS_PROTOCOL } from '../protocol';
import type { HandlerDefinition } from '../handler/handler';
import { dispatchToHandler } from '../transport/dispatch';
import type { Message } from '../topic/message';
import { createTransportMessage, withDrainTimeout } from '../transport/message-utils';
import type { Transport, Envelope } from '../transport/transport';

interface SubscribeResponse {
  subscriberId: string;
}

interface PullRequest {
  subscriberId: string;
  timeoutMs?: number;
}

interface PullResponse {
  deliveryId: string;
  message: {
    id: string;
    topic: string;
    payload: unknown;
    key?: string;
    dedupeKey?: string;
    topicVersion?: string;
    timestamp: string;
    attributes: Record<string, string>;
    attempt: number;
    traceId?: string;
  };
}

interface AckRequest {
  subscriberId: string;
  deliveryId: string;
  status: 'ack' | 'nack';
  reason?: string;
}

interface UnsubscribeRequest {
  subscriberId: string;
}

interface RemoteSubscription {
  id: string;
  definition: HandlerDefinition;
  callback: (message: Message<unknown>) => Promise<void>;
  pump?: Promise<void>;
  abortController?: AbortController;
}

/**
 * Transport client for a shared local MemoryServer.
 *
 * Used when another process already owns the local events server. This allows
 * multiple services to share a single local broker instead of each creating an
 * isolated in-process MemoryBroker.
 */
export interface LocalServerTransportOptions {
  /**
   * Maximum time (ms) to wait for in-flight pull loops to settle during
   * {@link LocalServerTransport.stop}. After this deadline `stop()` returns even
   * if a handler is still running. Default: 10_000.
   */
  drainTimeout?: number;
}

/**
 * Validate that the events endpoint uses an http(s) scheme before it flows into
 * fetch. Mirrors the SSRF guard the client transports apply, so a non-http
 * (file:, gopher:, cloud-metadata) or malformed endpoint cannot reach the
 * network layer.
 */
function assertHttpUrl(url: string, source: string): void {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    throw new Error(`Invalid ${source}: "${url}" is not a valid URL`);
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error(`Invalid ${source} scheme: only http:// and https:// are allowed, got "${parsed.protocol}"`);
  }
}

export class LocalServerTransport implements Transport {
  private readonly baseUrl: string;
  private readonly authToken: string | undefined;
  private readonly subscriptions: RemoteSubscription[] = [];
  private readonly drainTimeout: number;
  private running = false;
  private scopeFactory?: () => Promise<DetachedScope>;

  constructor(baseUrl: string, authToken?: string, options?: LocalServerTransportOptions) {
    assertHttpUrl(baseUrl, 'EVENTS_ENDPOINT');
    this.baseUrl = baseUrl.endsWith('/') ? baseUrl.slice(0, -1) : baseUrl;
    this.authToken = authToken;
    this.drainTimeout = Math.max(0, options?.drainTimeout ?? 10_000);
  }

  setScopeFactory(factory: () => Promise<DetachedScope>): void {
    this.scopeFactory = factory;
  }

  async publish(_topic: string, envelope: Envelope): Promise<void> {
    await this.request('/publish', {
      method: 'POST',
      body: JSON.stringify(envelope),
      headers: { 'Content-Type': 'application/json' },
    });
  }

  async subscribe(
    definition: HandlerDefinition,
    callback: (message: Message<unknown>) => Promise<void>,
  ): Promise<void> {
    const response = await this.request<SubscribeResponse>('/subscribe', {
      method: 'POST',
      body: JSON.stringify({
        topic: definition.topic.name,
        options: definition.options,
        filter: definition.filter,
      }),
      headers: { 'Content-Type': 'application/json' },
    });

    const subscription: RemoteSubscription = {
      id: response.subscriberId,
      definition,
      callback,
    };

    this.subscriptions.push(subscription);
    if (this.running) {
      subscription.pump = this.runSubscription(subscription);
    }
  }

  async start(): Promise<void> {
    this.running = true;
    await Promise.all(
      this.subscriptions.map(async (subscription) => {
        subscription.pump = this.runSubscription(subscription);
        await Bun.sleep(0);
      }),
    );
  }

  async stop(): Promise<void> {
    this.running = false;

    const unsubscribeRequests = this.subscriptions.map(async (subscription) => {
      subscription.abortController?.abort();
      await this.request('/unsubscribe', {
        method: 'POST',
        body: JSON.stringify({ subscriberId: subscription.id } satisfies UnsubscribeRequest),
        headers: { 'Content-Type': 'application/json' },
      }).catch(() => undefined);
    });
    await Promise.all(unsubscribeRequests);

    // Bound the wait for in-flight pull loops so a stuck handler cannot hang
    // shutdown indefinitely (honors the configured drainTimeout).
    const pumps = Promise.allSettled(this.subscriptions.map((subscription) => subscription.pump));
    await withDrainTimeout(pumps, this.drainTimeout);
  }

  private async runSubscription(subscription: RemoteSubscription): Promise<void> {
    while (this.running) {
      const abortController = new AbortController();
      subscription.abortController = abortController;

      let delivery: PullResponse | undefined;
      try {
        delivery = await this.pull(subscription.id, abortController.signal);
      } catch {
        if (!this.running) {
          break;
        }
        await Bun.sleep(50);
        continue;
      }

      if (!delivery) {
        continue;
      }

      const result = await this.handleDelivery(subscription, delivery.message);
      const reason = result.ok === false ? result.errorMessage : undefined;

      await this.request('/ack', {
        method: 'POST',
        body: JSON.stringify({
          subscriberId: subscription.id,
          deliveryId: delivery.deliveryId,
          status: result.ok ? 'ack' : 'nack',
          reason,
        } satisfies AckRequest),
        headers: { 'Content-Type': 'application/json' },
      }).catch(() => undefined);
    }
  }

  private async handleDelivery(
    subscription: RemoteSubscription,
    serialized: PullResponse['message'],
  ): Promise<{ ok: true } | { ok: false; errorMessage: string }> {
    const opts = subscription.definition.options;
    // The pull response carries the wire-serialized message fields. Rebuild a
    // minimal Envelope so message construction, timeout, and ack semantics go
    // through the same shared transport helpers as every other transport.
    const envelope: Envelope = {
      protocol: PUTNAMI_EVENTS_PROTOCOL,
      id: serialized.id,
      topic: serialized.topic,
      payload: serialized.payload,
      key: serialized.key,
      dedupeKey: serialized.dedupeKey,
      topicVersion: serialized.topicVersion,
      timestamp: serialized.timestamp,
      attributes: serialized.attributes,
      attempt: serialized.attempt,
      traceId: serialized.traceId,
    };

    const { message, ackState, abortController } = createTransportMessage(envelope, opts);

    try {
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
      return { ok: true };
    } catch (error) {
      return { ok: false, errorMessage: error instanceof Error ? error.message : String(error) };
    }
  }

  private get authHeaders(): Record<string, string> {
    return this.authToken ? { Authorization: `Bearer ${this.authToken}` } : {};
  }

  private async pull(subscriberId: string, signal: AbortSignal): Promise<PullResponse | undefined> {
    const response = await fetch(`${this.baseUrl}/pull`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...this.authHeaders },
      body: JSON.stringify({ subscriberId, timeoutMs: 25_000 } satisfies PullRequest),
      signal,
    });

    if (response.status === 204) {
      return undefined;
    }

    if (!response.ok) {
      const body = await response.text();
      throw new Error(body || `Failed to pull messages: HTTP ${response.status}`);
    }

    return (await response.json()) as PullResponse;
  }

  private async request<T = void>(path: string, init: RequestInit): Promise<T> {
    const headers = { ...((init.headers as Record<string, string>) ?? {}), ...this.authHeaders };
    const response = await fetch(`${this.baseUrl}${path}`, { ...init, headers });
    if (!response.ok) {
      const body = await response.text();
      throw new Error(body || `Request failed: ${path} (${response.status})`);
    }

    if (response.status === 204) {
      return undefined as T;
    }

    return (await response.json()) as T;
  }
}
