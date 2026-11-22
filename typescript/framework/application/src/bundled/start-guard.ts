import { useLogger } from '@putnami/runtime';
import type { Application } from '../application/application';

/**
 * A check a compiled entrypoint runs on the application it serves, after the
 * application graph is built and before any plugin starts.
 */
export type BundledStartGuard = (app: Application) => void | Promise<void>;

let pendingGuard: BundledStartGuard | undefined;

/**
 * Registers the guard the next {@link Application.start} runs before it
 * prepares the application. The generated bundled serve entrypoint registers
 * one so that, whether it builds the application itself or runs the project's
 * own `src/serve.ts`, the started application registers its contributed
 * configs and proves the capability manifest's config definitions were
 * activated. The guard runs once: the first start takes it.
 *
 * @internal Generated bundled entrypoints call this before startup.
 */
export function registerBundledStartGuard(guard: BundledStartGuard): void {
  pendingGuard = guard;
}

/**
 * Runs and clears the registered start guard, if any.
 * @internal Called by {@link Application.start}.
 */
export async function runBundledStartGuard(app: Application): Promise<void> {
  const guard = pendingGuard;
  pendingGuard = undefined;
  await guard?.(app);
}

/**
 * Logs a warning when the registered start guard has not run once the
 * project's serve entry has evaluated: no `Application` of this package had
 * started by then. The guard stays registered, so an application the entry
 * starts later still runs it.
 *
 * @internal Generated bundled entrypoints call this once their serve entry
 * has evaluated.
 */
export function reportPendingBundledStartGuard(): void {
  if (!pendingGuard) {
    return;
  }
  useLogger('putnami').warn(
    'no Application had started when the serve entry finished: contributed configs and the capability manifest config definitions are checked only when one starts',
  );
}
