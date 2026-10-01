const resetHooks: Array<() => void> = [];

export function registerConfigLoaderResetHook(reset: () => void): void {
  if (!resetHooks.includes(reset)) {
    resetHooks.push(reset);
  }
}

export function runConfigLoaderResetHooks(): void {
  for (const reset of resetHooks) {
    reset();
  }
}
