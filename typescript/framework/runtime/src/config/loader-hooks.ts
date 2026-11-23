const resetHooks: Array<() => void> = [];

let revision = 0;

export function registerConfigLoaderResetHook(reset: () => void): void {
  if (!resetHooks.includes(reset)) {
    resetHooks.push(reset);
  }
}

export function runConfigLoaderResetHooks(): void {
  bumpConfigRevision();
  for (const reset of resetHooks) {
    reset();
  }
}

/**
 * A number that changes whenever the config a `useConfig()` or
 * `useRawConfigSection()` call reads may change: the loader is reset, a
 * `ConfigService` is invalidated, or the active `ConfigService` changes.
 * A consumer that derives a value from config once and reuses it compares the
 * revision it derived the value under with the current one, and derives it
 * again when they differ.
 */
export function configRevision(): number {
  return revision;
}

/** Marks the config a `useConfig()` call reads as possibly changed. @internal */
export function bumpConfigRevision(): void {
  revision++;
}
