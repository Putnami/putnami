import type { useLogger } from '@putnami/runtime';

type Logger = ReturnType<typeof useLogger>;

/**
 * Maximum time to wait for graceful shutdown after SIGTERM/SIGINT before the
 * process force-exits. Sized below typical orchestrator grace periods (Cloud
 * Run ~10s, Kubernetes default 30s) so the framework exits on its own terms.
 */
export const SHUTDOWN_TIMEOUT_MS = 10_000;

/**
 * Install SIGTERM/SIGINT handlers that drive a graceful shutdown via `stop`.
 *
 * - Exits `0` on a clean stop, non-zero when `stop()` rejects, so an
 *   orchestrator sees a failed shutdown instead of a false success.
 * - Force-exits non-zero if `stop()` hangs past {@link SHUTDOWN_TIMEOUT_MS},
 *   rather than blocking until SIGKILL. The timer is unref'd so it never by
 *   itself keeps the process alive.
 *
 * @returns a cleanup function that removes the installed signal listeners.
 */
export function installSignalHandlers(stop: () => Promise<void>, logger: Logger): () => void {
  let stopping = false;
  const handler = (signal: string) => {
    if (stopping) return;
    stopping = true;
    logger.debug(`Received ${signal}`);

    const forceExit = setTimeout(() => {
      logger.error(`Graceful shutdown timed out after ${SHUTDOWN_TIMEOUT_MS}ms; forcing exit`);
      process.exit(1);
    }, SHUTDOWN_TIMEOUT_MS);
    if (typeof forceExit === 'object' && 'unref' in forceExit) {
      forceExit.unref();
    }

    stop().then(
      () => {
        clearTimeout(forceExit);
        process.exit(0);
      },
      (error) => {
        clearTimeout(forceExit);
        logger.error('Error during graceful shutdown:', error);
        process.exit(1);
      },
    );
  };
  const onSigterm = () => handler('SIGTERM');
  const onSigint = () => handler('SIGINT');
  process.on('SIGTERM', onSigterm);
  process.on('SIGINT', onSigint);
  return () => {
    process.off('SIGTERM', onSigterm);
    process.off('SIGINT', onSigint);
  };
}
