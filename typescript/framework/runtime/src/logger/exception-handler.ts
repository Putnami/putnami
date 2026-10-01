import type { Logger } from './logger';

/**
 * Installs global process-level exception handlers that log through the
 * application logger. Because the Logger reads traceId / logContext from
 * AsyncLocalStorage, errors that originate inside a `runInContext` scope
 * automatically include the full request/event context.
 *
 * @returns A cleanup function that removes the handlers (call during shutdown).
 */
export function installExceptionHandler(logger: Logger): () => void {
  const onUncaught = (error: Error) => {
    logger.error('Uncaught exception', error);
  };

  const onUnhandled = (reason: unknown) => {
    const error = reason instanceof Error ? reason : new Error(String(reason));
    logger.error('Unhandled rejection', error);
  };

  process.on('uncaughtException', onUncaught);
  process.on('unhandledRejection', onUnhandled);

  return () => {
    process.off('uncaughtException', onUncaught);
    process.off('unhandledRejection', onUnhandled);
  };
}
