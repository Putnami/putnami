import type { ResolvedMap, SchemaDefinition, TagSelector, Token } from '@putnami/runtime';
import { tokenName } from '@putnami/runtime';
import { getExternalCaller, getProjectRoot } from '@putnami/utils';
import { useContainer, resolveInjection } from '@putnami/runtime/inject';
import type { TopicDefinition, InferTopicPayload } from '../topic/topic';
import type {
  HandlerOptions,
  HandlerFn,
  InjectedHandlerFn,
  AttributeFilter,
  ResolvedHandlerOptions,
} from './handler.type';
import { resolveHandlerOptions } from './handler.type';

// ---------------------------------------------------------------------------
// HandlerDefinition — the object produced by handler().handle()
// ---------------------------------------------------------------------------

const HANDLER_MARKER = 'putnami:event-handler' as const;

/**
 * A fully resolved event handler definition.
 * Produced by the `handler()` builder. Consumed by the events plugin.
 */
export interface HandlerDefinition<S extends SchemaDefinition = SchemaDefinition> {
  readonly __handler: typeof HANDLER_MARKER;
  readonly topic: TopicDefinition<S>;
  readonly options: ResolvedHandlerOptions;
  readonly filter?: AttributeFilter;
  readonly inject?: Record<string, Token | TagSelector>;
  // biome-ignore lint/suspicious/noExplicitAny: payload type varies per topic; unknown would reject payload-typed handlers
  readonly handler: HandlerFn<any>;
  /** @internal Native declaration facts for build-time design discovery. */
  readonly dependencies?: readonly string[];
  readonly provenance?: { path: string; line?: number; symbol?: string };
}

export function isHandlerDefinition(value: unknown): value is HandlerDefinition {
  return typeof value === 'object' && value !== null && (value as HandlerDefinition).__handler === HANDLER_MARKER;
}

// ---------------------------------------------------------------------------
// HandlerBuilder — fluent API for configuring a handler
// ---------------------------------------------------------------------------

/**
 * Fluent builder for event handlers. Chain `.options()`, `.filter()`,
 * `.inject()`, and finalise with `.handle()`.
 *
 * @typeParam S - The SchemaDefinition from the topic
 * @typeParam T - The inferred payload type
 * @typeParam TInject - The injection map type (undefined if `.inject()` not called)
 */
export class HandlerBuilder<
  S extends SchemaDefinition,
  T = InferTopicPayload<TopicDefinition<S>>,
  TInject extends Record<string, Token | TagSelector> | undefined = undefined,
> {
  private _topic: TopicDefinition<S>;
  private _options?: HandlerOptions;
  private _filter?: AttributeFilter;
  private _inject?: Record<string, Token | TagSelector>;

  constructor(topic: TopicDefinition<S>) {
    this._topic = topic;
  }

  /**
   * Configure handler options (retries, distribution, timeout, etc.).
   *
   * @example
   * ```typescript
   * handler(UserCreated)
   *   .options({ maxRetries: 2, distribution: 'broadcast', timeout: 5_000 })
   *   .handle(async (msg) => { ... });
   * ```
   */
  options(opts: HandlerOptions): this {
    this._options = opts;
    return this;
  }

  /**
   * Apply a server-side attribute filter. Only messages whose attributes
   * match all specified key-value pairs will be delivered to this handler.
   *
   * @example
   * ```typescript
   * handler(OrderPlaced)
   *   .filter({ attributes: { region: 'eu' } })
   *   .handle(async (msg) => { ... });
   * ```
   */
  filter(filter: AttributeFilter): this {
    this._filter = filter;
    return this;
  }

  /**
   * Declare DI dependencies for this handler.
   * The handler function will receive the resolved dependencies as its first argument.
   *
   * Requires a DI scope to be active when the handler is invoked
   * (automatically provided when using the events plugin with an Application).
   *
   * @example
   * ```typescript
   * handler(UserCreated)
   *   .inject({ db: Database, notifier: NotificationService })
   *   .handle(async ({ db, notifier }, msg) => {
   *     const user = await db.query(`SELECT ...`);
   *     await notifier.send(user, msg.payload);
   *   });
   * ```
   */
  inject<M extends Record<string, Token | TagSelector>>(tokens: M): HandlerBuilder<S, T, M> {
    this._inject = tokens;
    return this as unknown as HandlerBuilder<S, T, M>;
  }

  /**
   * Provide the handler function — finalises the handler definition.
   *
   * In auto-ack mode (default):
   * - Returning normally acknowledges the message
   * - Throwing triggers retry (with backoff) or DLQ
   *
   * In manual-ack mode (`options({ ack: 'manual' })`):
   * - Call `msg.ack()` to acknowledge
   * - Call `msg.nack()` to trigger retry
   *
   * @example
   * ```typescript
   * export default handler(UserCreated)
   *   .handle(async (msg) => {
   *     console.log(msg.payload.email); // typed
   *     // return = ack
   *   });
   * ```
   */
  handle(
    fn: TInject extends Record<string, Token | TagSelector> ? InjectedHandlerFn<ResolvedMap<TInject>, T> : HandlerFn<T>,
  ): HandlerDefinition<S> {
    let finalHandler: HandlerFn<unknown>;

    if (this._inject) {
      const tokens = this._inject;
      const injected = fn as (deps: unknown, msg: unknown) => unknown;
      finalHandler = async (msg) => {
        const scope = useContainer();
        const deps = resolveInjection(tokens, scope);
        await injected(deps, msg);
      };
    } else {
      finalHandler = fn as HandlerFn<unknown>;
    }

    const caller = getExternalCaller(getProjectRoot());
    return {
      __handler: HANDLER_MARKER,
      topic: this._topic,
      options: resolveHandlerOptions(this._options),
      filter: this._filter,
      inject: this._inject,
      handler: finalHandler,
      ...(this._inject ? { dependencies: Object.values(this._inject).map(tokenName).sort() } : {}),
      ...(caller
        ? {
            provenance: {
              path: caller.filePath,
              line: caller.lineNumber,
              ...(caller.functionName ? { symbol: caller.functionName } : {}),
            },
          }
        : {}),
    };
  }
}

// ---------------------------------------------------------------------------
// handler() — entry point
// ---------------------------------------------------------------------------

/**
 * Define an event handler for a topic.
 *
 * Returns a builder that lets you configure options and filters
 * before providing the handler function.
 *
 * @param topic - The TopicDefinition to subscribe to
 * @returns A HandlerBuilder for fluent configuration
 *
 * @example
 * ```typescript
 * import { handler } from '@putnami/events';
 * import { UserCreated } from '../shared/topics';
 *
 * export default handler(UserCreated)
 *   .options({ maxRetries: 5, distribution: 'broadcast' })
 *   .handle(async (msg) => {
 *     console.log(msg.payload.name);
 *   });
 * ```
 */
export function handler<S extends SchemaDefinition>(topic: TopicDefinition<S>): HandlerBuilder<S> {
  return new HandlerBuilder(topic);
}
