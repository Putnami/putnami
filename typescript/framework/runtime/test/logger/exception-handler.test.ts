import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { installExceptionHandler } from '../../src/logger/exception-handler';
import { MemoryLogger } from '../../src/logger/memory.logger';

describe('installExceptionHandler', () => {
  let logger: MemoryLogger;
  let cleanup: () => void;

  beforeEach(() => {
    logger = new MemoryLogger('handler-test');
  });

  afterEach(() => {
    cleanup?.();
  });

  it('should log uncaught exceptions', () => {
    cleanup = installExceptionHandler(logger);
    const error = new Error('uncaught boom');
    process.emit('uncaughtException', error);

    expect(logger.entries).toHaveLength(1);
    expect(logger.entries[0].level).toBe('error');
    expect(logger.entries[0].message).toContain('Uncaught exception');
  });

  it('should log unhandled rejections with Error', () => {
    cleanup = installExceptionHandler(logger);
    const error = new Error('rejected');
    process.emit('unhandledRejection', error, Promise.resolve());

    expect(logger.entries).toHaveLength(1);
    expect(logger.entries[0].level).toBe('error');
    expect(logger.entries[0].message).toContain('Unhandled rejection');
  });

  it('should wrap non-Error rejections', () => {
    cleanup = installExceptionHandler(logger);
    process.emit('unhandledRejection', 'string reason', Promise.resolve());

    expect(logger.entries).toHaveLength(1);
    expect(logger.entries[0].level).toBe('error');
    expect(logger.entries[0].message).toContain('Unhandled rejection');
  });

  it('should remove handlers on cleanup', () => {
    cleanup = installExceptionHandler(logger);
    cleanup();

    // After cleanup, new events should not be logged
    const error = new Error('after cleanup');
    process.emit('uncaughtException', error);

    expect(logger.entries).toHaveLength(0);
  });
});
