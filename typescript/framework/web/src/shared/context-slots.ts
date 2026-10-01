import type { DocumentMeta } from '../client/document/document.types';

/**
 * Typed accessors for the cross-cutting per-request context slots the
 * SSR pipeline shares between modules (renderer, static render, the SSR
 * document helper, security middleware).
 *
 * These slots live on the request context object under well-known keys.
 * Always go through these helpers instead of casting the context to a
 * record with inline string keys — a key rename must be a single-place
 * change, not a silent break in whichever module still uses the old
 * literal.
 *
 * This module is browser-bundle safe: it has no server-only imports.
 */

/** Request-context slot holding the shared per-request DocumentMeta. */
export const DOCUMENT_META_CONTEXT_KEY = 'documentMeta';

/** Views an opaque context object as its untyped slot record. */
export function contextSlots(ctx: object): Record<string, unknown> {
  return ctx as unknown as Record<string, unknown>;
}

/**
 * Returns the request's shared DocumentMeta, creating the slot on first
 * access so all readers mutate the same object.
 */
export function ensureDocumentMeta(ctx: object): DocumentMeta {
  const slots = contextSlots(ctx);
  slots[DOCUMENT_META_CONTEXT_KEY] ||= {};
  return slots[DOCUMENT_META_CONTEXT_KEY] as DocumentMeta;
}

/** Publishes the per-request CSP nonce under the given well-known key. */
export function setContextSlot(ctx: object, key: string, value: unknown): void {
  contextSlots(ctx)[key] = value;
}

/**
 * Request slot: data a server plugin wants serialized as
 * `window.__putnamiBootstrap` in the hydration script.
 */
export const CLIENT_BOOTSTRAP_CONTEXT_KEY = 'clientBootstrap';

/**
 * Request slot: module script URLs a server plugin wants emitted after the
 * hydrate script, nonce-stamped.
 */
export const CLIENT_SCRIPTS_CONTEXT_KEY = 'clientScripts';

/**
 * Request slot the page renderer sets once it has emitted the bootstrap and
 * the client scripts into the document it is streaming.
 *
 * A statically served page never reaches the renderer — the handler reads
 * pre-rendered bytes — so the slot stays unset and a plugin that needs its
 * data in the browser knows the document it is looking at carries none of it.
 */
export const CLIENT_BOOTSTRAP_EMITTED_CONTEXT_KEY = 'clientBootstrapEmitted';

/** Request slot written by the action handler once the action settled. */
export const ACTION_OUTCOME_CONTEXT_KEY = 'actionOutcome';

/** How an action settled, from the caller's point of view. */
export type ActionOutcome = 'ok' | 'validation_error' | 'error';

/** What the action handler publishes under {@link ACTION_OUTCOME_CONTEXT_KEY}. */
export interface ActionOutcomeSlot {
  /** The matched route pattern, or undefined when nothing matched. */
  route: string | undefined;
  outcome: ActionOutcome;
}

/**
 * Shallow-merges `entry` into the bootstrap slot (later writers win per
 * top-level key). The slot is generic: the renderer serializes whatever is in
 * it and never inspects the keys.
 */
export function mergeClientBootstrap(ctx: object, entry: Record<string, unknown>): void {
  const slots = contextSlots(ctx);
  const current = (slots[CLIENT_BOOTSTRAP_CONTEXT_KEY] as Record<string, unknown> | undefined) ?? {};
  slots[CLIENT_BOOTSTRAP_CONTEXT_KEY] = { ...current, ...entry };
}

/** Appends a module script URL to the client-scripts slot, deduplicated by URL. */
export function pushClientScript(ctx: object, url: string): void {
  const slots = contextSlots(ctx);
  const list = (slots[CLIENT_SCRIPTS_CONTEXT_KEY] as string[] | undefined) ?? [];
  if (!list.includes(url)) {
    list.push(url);
  }
  slots[CLIENT_SCRIPTS_CONTEXT_KEY] = list;
}

/** Returns the bootstrap slot, or undefined when no plugin wrote to it. */
export function readClientBootstrap(ctx: object): Record<string, unknown> | undefined {
  return contextSlots(ctx)[CLIENT_BOOTSTRAP_CONTEXT_KEY] as Record<string, unknown> | undefined;
}

/** Returns the client-scripts slot, empty when no plugin pushed a URL. */
export function readClientScripts(ctx: object): readonly string[] {
  return (contextSlots(ctx)[CLIENT_SCRIPTS_CONTEXT_KEY] as string[] | undefined) ?? [];
}

/** Records that the renderer wrote the bootstrap and client scripts into the document. */
export function markClientBootstrapEmitted(ctx: object): void {
  contextSlots(ctx)[CLIENT_BOOTSTRAP_EMITTED_CONTEXT_KEY] = true;
}

/**
 * Reports whether the response for this request was rendered with the
 * bootstrap and client-script slots in it.
 *
 * False for a statically served page, for a JSON route, and for anything a
 * plugin returned without the React renderer.
 */
export function clientBootstrapEmitted(ctx: object): boolean {
  return contextSlots(ctx)[CLIENT_BOOTSTRAP_EMITTED_CONTEXT_KEY] === true;
}
