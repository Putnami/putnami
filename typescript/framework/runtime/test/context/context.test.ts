import { describe, expect, it } from 'bun:test';
import { runInContext, tryContext, useContext } from '../../src/context';

describe('context.utils', () => {
  describe('useContext', () => {
    it('should throw when called outside runInContext', () => {
      expect(() => useContext()).toThrow('useContext() called outside of runInContext()');
    });

    it('should return the context inside runInContext', async () => {
      const ctx = { traceId: 'test-123' };
      await runInContext(ctx, () => {
        const result = useContext<typeof ctx>();
        expect(result.traceId).toBe('test-123');
      });
    });
  });

  describe('tryContext', () => {
    it('should return undefined outside runInContext', () => {
      expect(tryContext()).toBeUndefined();
    });

    it('should return the context inside runInContext', async () => {
      const ctx = { traceId: 'try-456' };
      await runInContext(ctx, () => {
        const result = tryContext<typeof ctx>();
        expect(result?.traceId).toBe('try-456');
      });
    });
  });

  describe('runInContext', () => {
    it('should support async actions', async () => {
      const result = await runInContext({ traceId: 'async-1' }, async () => useContext<{ traceId: string }>().traceId);
      expect(result).toBe('async-1');
    });
  });
});
