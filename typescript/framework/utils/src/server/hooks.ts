/**
 * Hook Protocol (JSONL) — Server Only
 *
 * The JSONL event protocol and command runner used by Putnami extension hooks
 * (e.g. `@putnami/application` and `@putnami/web` generate hooks). This is a
 * specialized SDK kept off the generic `@putnami/utils` root barrel; import it
 * from the dedicated subpath:
 *
 * ```ts
 * import { runHookCommand, standardHookModel } from '@putnami/utils/hooks';
 * ```
 *
 * @module @putnami/utils/hooks
 */

import './env.declare';

// Hook event protocol (JSONL)
export {
  createEvent,
  emitArtifact,
  emitError,
  emitEvent,
  emitEventFlushed,
  emitLog,
  emitProgress,
  emitSummary,
  type HookContext,
  HookContextSchema,
  type HookEvent,
  type HookEventArtifact,
  type HookEventBase,
  type HookEventError,
  type HookEventLevel,
  type HookEventLog,
  type HookEventMeta,
  type HookEventMetric,
  type HookEventProgress,
  type HookEventSummary,
  type HookEventType,
  type HookExitCode,
  HookExitCodes,
  parseEvent,
  validateHookContext,
} from './hook-events';
// Hook command runner
export {
  type HookCommandOptions,
  type HookFlagDefinition,
  type HookModelDescriptor,
  type HookResult,
  readHookContext,
  runHookCommand,
  standardHookModel,
  type StandardHookOptions,
} from './hook-command';
