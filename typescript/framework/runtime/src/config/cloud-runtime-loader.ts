import { createRequire } from 'node:module';

export const CLOUD_RUNTIME_SPECIFIER = '@putnami/cloud/runtime';

const runtimeRequire = createRequire(import.meta.url);

export function loadCloudRuntimeModule<T>(): T {
  return runtimeRequire(CLOUD_RUNTIME_SPECIFIER) as T;
}

export function isMissingOptionalCloudRuntime(error: unknown): boolean {
  // Classify by shape, not class. The workspace's toolchain floor is Bun 1.4,
  // where the resolver's `ResolveMessage` subclasses `Error` — but runtimes
  // below the floor (CI runner images, user environments) still throw it as a
  // non-Error value, and a missing OPTIONAL module must never crash there.
  // Duck-typing on a string `message`/`code` accepts both shapes.
  if (!isResolutionErrorLike(error)) {
    return false;
  }

  const { code, message } = error;
  if (code !== 'MODULE_NOT_FOUND' && code !== 'ERR_MODULE_NOT_FOUND' && !message.includes('Cannot find module')) {
    return false;
  }

  const firstLine = message.split('\n', 1)[0] ?? '';
  return (
    firstLine.includes(quoted(CLOUD_RUNTIME_SPECIFIER, "'")) || firstLine.includes(quoted(CLOUD_RUNTIME_SPECIFIER, '"'))
  );
}

type ResolutionErrorLike = { message: string; code?: unknown };

function isResolutionErrorLike(error: unknown): error is ResolutionErrorLike {
  return typeof error === 'object' && error !== null && typeof (error as { message?: unknown }).message === 'string';
}

function quoted(value: string, quote: '"' | "'"): string {
  return `${quote}${value}${quote}`;
}
