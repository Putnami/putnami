import { describe, expect, it } from 'bun:test';
import { TaskQueue } from '../src';

describe('task.utils', () => {
  describe('TaskQueue', () => {
    it('processes single task', async () => {
      const queue = new TaskQueue(2);
      const result = await queue.process(async () => 'done');
      expect(result).toBe('done');
    });

    it('processes multiple tasks concurrently', async () => {
      const queue = new TaskQueue(3);
      const order: number[] = [];

      const results = await Promise.all([
        queue.process(async () => {
          await sleep(10);
          order.push(1);
          return 1;
        }),
        queue.process(async () => {
          await sleep(5);
          order.push(2);
          return 2;
        }),
        queue.process(async () => {
          order.push(3);
          return 3;
        }),
      ]);

      expect(results.sort()).toEqual([1, 2, 3]);
      expect(order.sort((a, b) => a - b)).toEqual([1, 2, 3]);
    });

    it('respects concurrency limit', async () => {
      const queue = new TaskQueue(2);
      let running = 0;
      let maxRunning = 0;

      const task = async () => {
        running++;
        maxRunning = Math.max(maxRunning, running);
        await sleep(10);
        running--;
        return true;
      };

      await Promise.all([queue.process(task), queue.process(task), queue.process(task), queue.process(task)]);

      expect(maxRunning).toBe(2);
    });

    it('handles task errors', async () => {
      const queue = new TaskQueue(1);

      await expect(
        queue.process(async () => {
          throw new Error('Task failed');
        }),
      ).rejects.toThrow('Task failed');
    });

    it('continues processing after task error', async () => {
      const queue = new TaskQueue(1);

      try {
        await queue.process(async () => {
          throw new Error('First task failed');
        });
      } catch {
        // Expected error
      }

      const result = await queue.process(async () => 'recovered');
      expect(result).toBe('recovered');
    });
  });
});

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
