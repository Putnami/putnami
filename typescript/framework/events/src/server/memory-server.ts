import { randomUUID } from 'node:crypto';
import { writeFileSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { useLogger } from '@putnami/runtime';
import type { Message } from '../topic/message';
import type { Envelope } from '../transport/transport';
import { MemoryBroker } from './memory-broker';
import {
  isValidEnvelope,
  isValidSubscribeBody,
  sanitizeRemoteEnvelope,
  toHandlerDefinition,
  type RemoteAckBody,
  type RemotePullBody,
  type RemoteSubscribeBody,
  type RemoteUnsubscribeBody,
} from './remote-protocol';
import { RemoteSubscriberRegistry } from './remote-subscribers';

// ---------------------------------------------------------------------------
// MemoryServer — local HTTP server for cross-service event exchange
// ---------------------------------------------------------------------------

const DEFAULT_PORT = 4222;
const LONG_POLL_TIMEOUT_MS = 25_000;
const MAX_PUBLISH_BYTES = 1_048_576;

interface ServerOptions {
  /** Port for the local events server. Default: 4222 */
  port?: number;
  /** Drain timeout passed to the in-memory broker. Default: 10_000 */
  drainTimeout?: number;
  /**
   * Forward to the in-memory broker to deliberately redeliver ~2% of messages
   * and surface non-idempotent handlers. Default: false.
   */
  simulateDuplicates?: boolean;
}

/**
 * A lightweight HTTP server that wraps the in-memory broker,
 * enabling cross-service event exchange during local development.
 *
 * Protocol:
 * - POST /publish      — publish an envelope
 * - POST /subscribe    — register a remote subscriber
 * - POST /pull         — long-poll for the next assigned delivery
 * - POST /ack          — acknowledge or reject a delivery
 * - POST /unsubscribe  — remove a remote subscriber
 * - GET  /health       — readiness check
 *
 * In local dev, the events plugin auto-starts this server if no
 * EVENTS_ENDPOINT is configured. Multiple services connect to it.
 */
export class MemoryServer {
  private readonly broker: MemoryBroker;
  private readonly port: number;
  private readonly authToken: string;
  private readonly subscribers: RemoteSubscriberRegistry;
  private server: ReturnType<typeof Bun.serve> | undefined;

  constructor(options?: ServerOptions) {
    this.port = options?.port ?? DEFAULT_PORT;
    this.broker = new MemoryBroker({
      simulateDuplicates: options?.simulateDuplicates ?? false,
      drainTimeout: options?.drainTimeout,
    });
    this.authToken = randomUUID();
    this.subscribers = new RemoteSubscriberRegistry((topic, callback) => this.broker.unsubscribe(topic, callback));
  }

  /** Get the auth token required to access this server. */
  getAuthToken(): string {
    return this.authToken;
  }

  getBroker(): MemoryBroker {
    return this.broker;
  }

  getPort(): number {
    return this.server?.port ?? this.port;
  }

  async start(): Promise<void> {
    if (this.server) {
      return;
    }

    const logger = useLogger('events:server');

    await this.broker.start();

    try {
      this.server = Bun.serve({
        port: this.port,
        hostname: '127.0.0.1',
        fetch: async (req) => this.handleRequest(req),
      });
    } catch (error) {
      await this.broker.stop().catch(() => undefined);
      throw error;
    }

    // Write token to temp file for cross-process discovery
    this.writeTokenFile();

    logger.debug(`Local events server listening on http://localhost:${this.getPort()}`);
  }

  async stop(): Promise<void> {
    const tokenPort = this.getPort();
    await this.broker.stop();
    this.server?.stop();
    this.server = undefined;
    this.removeTokenFile(tokenPort);

    this.subscribers.clear('Server stopping');
  }

  private async handleRequest(req: Request): Promise<Response> {
    const url = new URL(req.url);

    // Health check is public — no auth required
    if (url.pathname === '/health' && req.method === 'GET') {
      return Response.json({ status: 'ok', service: 'putnami-events' });
    }

    // All other endpoints require auth
    if (!this.validateAuth(req)) {
      return Response.json({ error: 'Unauthorized' }, { status: 401 });
    }

    if (req.method === 'POST') {
      switch (url.pathname) {
        case '/publish':
          return this.handlePublish(req);
        case '/subscribe':
          return this.handleSubscribe(req);
        case '/pull':
          return this.handlePull(req);
        case '/ack':
          return this.handleAck(req);
        case '/unsubscribe':
          return this.handleUnsubscribe(req);
      }
    }

    return new Response('Not Found', { status: 404 });
  }

  private async handlePublish(req: Request): Promise<Response> {
    try {
      // Reject oversized payloads before parsing (DoS protection)
      const contentLength = Number(req.headers.get('content-length') ?? 0);
      if (contentLength > MAX_PUBLISH_BYTES) {
        return Response.json({ error: 'Payload too large' }, { status: 413 });
      }

      const envelope = (await req.json()) as Envelope;
      if (!isValidEnvelope(envelope)) {
        return Response.json({ error: 'Invalid envelope structure' }, { status: 400 });
      }

      // Strip caller-supplied reserved attributes (auth.*) before publishing.
      // The shared bearer token authorizes access to the server, not a specific
      // end-user identity, so any auth.* arriving over the wire is unverifiable
      // and must never be trusted by downstream handlers as authenticated
      // identity. Mirrors the in-process anti-spoofing filter in buildEnvelope.
      sanitizeRemoteEnvelope(envelope);

      await this.broker.publish(envelope.topic, envelope);
      return Response.json({ ok: true });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return Response.json({ error: message }, { status: 500 });
    }
  }

  private async handleSubscribe(req: Request): Promise<Response> {
    try {
      const body = (await req.json()) as Partial<RemoteSubscribeBody>;
      if (!isValidSubscribeBody(body)) {
        return Response.json({ error: 'Invalid subscriber definition' }, { status: 400 });
      }

      const subscriberId = randomUUID();
      const callback = async (message: Message<unknown>) =>
        await this.subscribers.enqueue(subscriberId, body.options, message);

      await this.broker.subscribe(toHandlerDefinition(body), callback);
      this.subscribers.register(subscriberId, body.topic, callback);

      return Response.json({ subscriberId });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return Response.json({ error: message }, { status: 500 });
    }
  }

  private async handlePull(req: Request): Promise<Response> {
    try {
      const body = (await req.json()) as Partial<RemotePullBody>;
      if (typeof body.subscriberId !== 'string' || body.subscriberId.length === 0) {
        return Response.json({ error: 'Invalid subscriber id' }, { status: 400 });
      }

      const timeoutMs = Math.min(Math.max(body.timeoutMs ?? LONG_POLL_TIMEOUT_MS, 1), LONG_POLL_TIMEOUT_MS);
      const result = await this.subscribers.pull(body.subscriberId, timeoutMs);

      if (result.status === 'not-found') {
        return Response.json({ error: 'Subscriber not found' }, { status: 404 });
      }
      if (result.status === 'concurrent') {
        return Response.json({ error: 'Concurrent pull is not allowed' }, { status: 409 });
      }
      if (result.status === 'empty') {
        return new Response(null, { status: 204 });
      }

      return Response.json(result.delivery);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return Response.json({ error: message }, { status: 500 });
    }
  }

  private async handleAck(req: Request): Promise<Response> {
    try {
      const body = (await req.json()) as Partial<RemoteAckBody>;
      if (
        typeof body.subscriberId !== 'string' ||
        typeof body.deliveryId !== 'string' ||
        (body.status !== 'ack' && body.status !== 'nack')
      ) {
        return Response.json({ error: 'Invalid acknowledgement payload' }, { status: 400 });
      }

      const result = this.subscribers.ack(body.subscriberId, body.deliveryId, body.status, body.reason);
      if (result === 'subscriber-not-found') {
        return Response.json({ error: 'Subscriber not found' }, { status: 404 });
      }
      if (result === 'delivery-not-found') {
        return Response.json({ error: 'Delivery not found' }, { status: 404 });
      }

      return Response.json({ ok: true });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return Response.json({ error: message }, { status: 500 });
    }
  }

  private async handleUnsubscribe(req: Request): Promise<Response> {
    try {
      const body = (await req.json()) as Partial<RemoteUnsubscribeBody>;
      if (typeof body.subscriberId !== 'string' || body.subscriberId.length === 0) {
        return Response.json({ error: 'Invalid subscriber id' }, { status: 400 });
      }

      this.subscribers.remove(body.subscriberId, 'Remote subscriber disconnected');
      return Response.json({ ok: true });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return Response.json({ error: message }, { status: 500 });
    }
  }

  private validateAuth(req: Request): boolean {
    const header = req.headers.get('authorization');
    if (!header) return false;
    const token = header.startsWith('Bearer ') ? header.slice(7) : '';
    return token === this.authToken;
  }

  private writeTokenFile(): void {
    try {
      writeFileSync(MemoryServer.tokenFilePath(this.getPort()), this.authToken, { encoding: 'utf-8', mode: 0o600 });
    } catch {
      // Non-fatal — cross-process discovery will fail but same-process works
    }
  }

  private removeTokenFile(port = this.getPort()): void {
    try {
      unlinkSync(MemoryServer.tokenFilePath(port));
    } catch {
      // Ignore if already removed
    }
  }

  /** File path where the auth token is stored for cross-process discovery. */
  static tokenFilePath(port = DEFAULT_PORT): string {
    return join(tmpdir(), `putnami-events-${port}.token`);
  }

  /** Read the auth token from the temp file (for cross-process clients). */
  static async readAuthToken(port = DEFAULT_PORT): Promise<string | undefined> {
    try {
      const file = Bun.file(MemoryServer.tokenFilePath(port));
      if (await file.exists()) {
        return (await file.text()).trim();
      }
    } catch {
      // Token file not available
    }
    return undefined;
  }

  /** Check if a local events server is already running on the given port */
  static async isRunning(port = DEFAULT_PORT): Promise<boolean> {
    try {
      const res = await fetch(`http://localhost:${port}/health`, {
        signal: AbortSignal.timeout(500),
      });
      const body = (await res.json()) as { status: string; service?: string };
      return body.status === 'ok' && body.service === 'putnami-events';
    } catch {
      return false;
    }
  }
}

export const EVENTS_DEFAULT_PORT = DEFAULT_PORT;
