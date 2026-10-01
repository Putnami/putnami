import type { ContainerContext, useLogger } from '@putnami/runtime';
import type { Module } from './module';
import type { Plugin } from './module.types';

type Logger = ReturnType<typeof useLogger>;

/**
 * Run shutdown hooks in reverse order, logging (but not rethrowing) failures so
 * one bad hook does not abort the rest of the drain.
 */
export async function runShutdownHooks(hooks: Array<() => Promise<void>>, logger: Logger): Promise<void> {
  for (const hook of [...hooks].reverse()) {
    try {
      await hook();
    } catch (error) {
      logger.error('Error in shutdown hook:', error);
    }
  }
}

/**
 * Stop plugins in reverse tree order, logging (but not rethrowing) failures so
 * every plugin gets a chance to release resources.
 */
export async function stopPlugins(plugins: Array<{ plugin: Plugin; owner: Module }>, logger: Logger): Promise<void> {
  for (const { plugin, owner } of [...plugins].reverse()) {
    try {
      await plugin.stop?.(owner);
    } catch (error) {
      logger.error('Error stopping plugin:', error);
    }
  }
}

/** Close the DI container context, logging any failure. */
export async function closeContainerContext(context: ContainerContext, logger: Logger): Promise<void> {
  try {
    await context.close();
  } catch (error) {
    logger.error('Error closing container context:', error);
  }
}
