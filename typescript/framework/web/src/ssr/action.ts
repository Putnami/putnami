import {
  applyInputValidation,
  type EndpointRequestContext,
  type HttpMiddleware,
  type HttpRequestContext,
  type InferSchema,
  type RouteHandlerResult,
  type SchemaDefinition,
  type SecurityGuard,
  SecurityMiddleware,
  type SecurityOptions,
} from '@putnami/application';
import type { ResolvedMap, TagSelector, Token } from '@putnami/runtime';
import { wrapWithMiddlewareRaw } from './route-middleware.utils';
import { resolveInjection, useContainer } from '@putnami/runtime/inject';

// ---------------------------------------------------------------------------
// EvictPattern — cache eviction pattern type
// ---------------------------------------------------------------------------

/**
 * Cache eviction pattern: either a static glob pattern or a function returning a pattern.
 * @example
 * 'loader:/users/*'           // Static pattern
 * () => `user:${getCurrentUserId()}`  // Dynamic pattern
 */
export type EvictPattern = string | (() => string);

// ---------------------------------------------------------------------------
// ActionDefinition — the object produced by action()
// ---------------------------------------------------------------------------

const ACTION_MARKER = 'putnami:action' as const;

export interface ActionDefinition {
  readonly __action: typeof ACTION_MARKER;
  readonly handler: (ctx: HttpRequestContext) => RouteHandlerResult | Promise<RouteHandlerResult>;
  readonly schemas?: {
    readonly params?: SchemaDefinition;
    readonly query?: SchemaDefinition;
    readonly body?: SchemaDefinition;
  };
  /** Cache patterns to evict after action executes successfully */
  readonly evict?: readonly EvictPattern[];
  readonly middleware?: readonly HttpMiddleware[];
}

export function isActionDefinition(value: unknown): value is ActionDefinition {
  return typeof value === 'object' && value !== null && (value as ActionDefinition).__action === ACTION_MARKER;
}

/**
 * Extract the return type of an action handler function.
 *
 * Since `ActionDefinition` erases generic type information, this utility
 * works with the handler function directly (before it's wrapped by `action()`).
 *
 * @example
 * ```typescript
 * // action.ts
 * const myHandler = async (ctx: EndpointRequestContext) => {
 *   return { success: true, id: ctx.params.id };
 * };
 * export default action(myHandler);
 * export type ActionData = InferActionData<typeof myHandler>;
 *
 * // page.tsx
 * import type { ActionData } from './action';
 * const data = useActionData<ActionData>();
 * ```
 */
export type InferActionData<THandler extends (...args: never[]) => unknown> = Awaited<ReturnType<THandler>>;

// ---------------------------------------------------------------------------
// ActionBuilder — fluent API built by action()
// ---------------------------------------------------------------------------

export class ActionBuilder<
  TParams = undefined,
  TQuery = undefined,
  TBody = undefined,
  TInject extends Record<string, Token | TagSelector> | undefined = undefined,
> {
  private _params?: SchemaDefinition;
  private _query?: SchemaDefinition;
  private _body?: SchemaDefinition;
  private _evict?: EvictPattern[];
  private _inject?: Record<string, Token | TagSelector>;
  private _middleware: HttpMiddleware[] = [];

  /** Declare and validate path parameters */
  params<S extends SchemaDefinition>(schema: S): ActionBuilder<InferSchema<S>, TQuery, TBody, TInject> {
    this._params = schema;
    return this as unknown as ActionBuilder<InferSchema<S>, TQuery, TBody, TInject>;
  }

  /** Declare and validate query string parameters */
  query<S extends SchemaDefinition>(schema: S): ActionBuilder<TParams, InferSchema<S>, TBody, TInject> {
    this._query = schema;
    return this as unknown as ActionBuilder<TParams, InferSchema<S>, TBody, TInject>;
  }

  /** Declare and validate request body */
  body<S extends SchemaDefinition>(schema: S): ActionBuilder<TParams, TQuery, InferSchema<S>, TInject> {
    this._body = schema;
    return this as unknown as ActionBuilder<TParams, TQuery, InferSchema<S>, TInject>;
  }

  /**
   * Evict cache patterns after action executes successfully.
   * Patterns can be static strings (with glob wildcards) or functions returning patterns.
   *
   * @example
   * // Static pattern
   * action().evict('loader:/users/*').handle(...)
   *
   * @example
   * // Multiple patterns including dynamic
   * action().evict('loader:/users/*', () => `user:${getId()}`).handle(...)
   */
  evict(...patterns: EvictPattern[]): ActionBuilder<TParams, TQuery, TBody, TInject> {
    this._evict = patterns;
    return this as unknown as ActionBuilder<TParams, TQuery, TBody, TInject>;
  }

  /**
   * Declare dependencies to resolve from the current DI scope. See the
   * `action()` entry-point doc for a worked example.
   */
  inject<M extends Record<string, Token | TagSelector>>(tokens: M): ActionBuilder<TParams, TQuery, TBody, M> {
    this._inject = tokens;
    return this as unknown as ActionBuilder<TParams, TQuery, TBody, M>;
  }

  /**
   * Require authentication and enforce access rules for this action.
   *
   * @example Require any authenticated user
   * ```ts
   * action().secure().handle(...)
   * ```
   *
   * @example Custom guard function
   * ```ts
   * action().secure((user, ctx) => user.orgId === ctx.params?.orgId).handle(...)
   * ```
   */
  secure(optionsOrGuard?: SecurityOptions | SecurityGuard): ActionBuilder<TParams, TQuery, TBody, TInject> {
    this._middleware.push(SecurityMiddleware(optionsOrGuard ?? {}));
    return this as unknown as ActionBuilder<TParams, TQuery, TBody, TInject>;
  }

  /** Provide the handler function — finalises the action definition */
  handle(
    handler: TInject extends Record<string, Token | TagSelector>
      ? (
          deps: ResolvedMap<TInject>,
          ctx: EndpointRequestContext<TParams, TQuery, TBody>,
        ) => RouteHandlerResult | Promise<RouteHandlerResult>
      : (ctx: EndpointRequestContext<TParams, TQuery, TBody>) => RouteHandlerResult | Promise<RouteHandlerResult>,
  ): ActionDefinition {
    const schemas =
      this._params || this._query || this._body
        ? { params: this._params, query: this._query, body: this._body }
        : undefined;
    const needsWrapping = !!schemas;

    type ActionHandler = (ctx: HttpRequestContext) => RouteHandlerResult | Promise<RouteHandlerResult>;
    let finalHandler: ActionHandler;

    if (this._inject) {
      const tokens = this._inject;
      const injectedHandler = handler as unknown as (deps: unknown, ctx: unknown) => RouteHandlerResult;
      const baseHandler: ActionHandler = async (ctx) => {
        const scope = useContainer();
        const deps = resolveInjection(tokens, scope);
        return injectedHandler(deps, ctx);
      };
      finalHandler = needsWrapping ? wrapActionWithValidation(baseHandler, schemas) : baseHandler;
    } else {
      const baseHandler = handler as unknown as ActionHandler;
      finalHandler = needsWrapping ? wrapActionWithValidation(baseHandler, schemas) : baseHandler;
    }

    if (this._middleware.length > 0) {
      finalHandler = wrapWithMiddlewareRaw(finalHandler, this._middleware);
    }

    const hasMiddleware = this._middleware.length > 0;
    return {
      __action: ACTION_MARKER,
      handler: finalHandler,
      schemas,
      evict: this._evict,
      middleware: hasMiddleware ? [...this._middleware] : undefined,
    };
  }
}

// ---------------------------------------------------------------------------
// action() — two-mode entry point
// ---------------------------------------------------------------------------

/**
 * Define a page action handler.
 *
 * **Simple mode** — pass a handler directly:
 * ```ts
 * export default action((ctx) => {
 *   return { success: true };
 * });
 * ```
 *
 * **Builder mode** — chain validation schemas:
 * ```ts
 * export default action()
 *   .params({ id: Uuid })
 *   .body({ name: String, email: Email })
 *   .handle(async (ctx) => {
 *     // ctx.params.id — validated as UUID
 *     // ctx.body.name — validated string
 *     return { updated: true };
 *   });
 * ```
 *
 * **With dependency injection:**
 * ```ts
 * export default action()
 *   .body({ name: String, email: Email })
 *   .inject({ userService: UserService })
 *   .handle(async ({ userService }, ctx) => {
 *     const body = await ctx.body();
 *     return userService.create(body);
 *   });
 * ```
 *
 * **With security:**
 * ```ts
 * export default action()
 *   .secure({ roles: ['editor'] })
 *   .body({ title: String })
 *   .handle(async (ctx) => {
 *     const body = await ctx.body();
 *     return createProject(body);
 *   });
 * ```
 */
export function action(
  handler: (ctx: HttpRequestContext) => RouteHandlerResult | Promise<RouteHandlerResult>,
): ActionDefinition;
export function action(): ActionBuilder;
export function action(
  handler?: (ctx: HttpRequestContext) => RouteHandlerResult | Promise<RouteHandlerResult>,
): ActionDefinition | ActionBuilder {
  if (handler) {
    return { __action: ACTION_MARKER, handler };
  }
  return new ActionBuilder();
}

// ---------------------------------------------------------------------------
// Internal
// ---------------------------------------------------------------------------

function wrapActionWithValidation(
  handler: (ctx: HttpRequestContext) => RouteHandlerResult | Promise<RouteHandlerResult>,
  schemas: ActionDefinition['schemas'],
): (ctx: HttpRequestContext) => RouteHandlerResult | Promise<RouteHandlerResult> {
  return async (ctx: HttpRequestContext) => {
    applyInputValidation(ctx, schemas);
    return handler(ctx);
  };
}
