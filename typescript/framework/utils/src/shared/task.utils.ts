/**
 * Task Processing Utility
 *
 * Browser and SSR compatible async task queue with concurrency control.
 * @module @putnami/utils
 */

/**
 * Represents an async task function that returns a promise.
 * @template T - The return type of the task
 */
type Task<T = unknown> = () => Promise<T>;

/**
 * A concurrent task processor that limits the number of simultaneous async operations.
 *
 * Useful for throttling API calls, file operations, or any async work that
 * shouldn't overwhelm resources.
 *
 * @example
 * ```typescript
 * // Create a queue with max 3 concurrent tasks
 * const queue = new TaskQueue(3);
 *
 * // Add tasks to the queue
 * const results = await Promise.all([
 *   queue.process(() => fetch('/api/user/1')),
 *   queue.process(() => fetch('/api/user/2')),
 *   queue.process(() => fetch('/api/user/3')),
 *   queue.process(() => fetch('/api/user/4')), // Waits for a slot
 *   queue.process(() => fetch('/api/user/5')), // Waits for a slot
 * ]);
 * ```
 */
export class TaskQueue {
  private readonly concurrency: number;
  private running = 0;
  private queue: Task[] = [];

  /**
   * Creates a new TaskQueue instance.
   *
   * @param concurrency - Maximum number of tasks that can run simultaneously (default: 10)
   */
  constructor(concurrency = 10) {
    this.concurrency = concurrency;
  }

  /**
   * Adds a task to the queue and returns a promise that resolves when the task completes.
   *
   * Tasks are executed in FIFO order when slots become available.
   *
   * @template T - The return type of the task
   * @param task - The async task function to execute
   * @returns A promise that resolves with the task result
   *
   * @example
   * ```typescript
   * const queue = new TaskQueue(2);
   *
   * const result = await queue.process(async () => {
   *   const response = await fetch('/api/data');
   *   return response.json();
   * });
   * ```
   */
  process<T>(task: Task<T>): Promise<T> {
    return new Promise((resolve, reject) => {
      this.queue.push(async () => {
        try {
          const result = await task();
          resolve(result);
        } catch (err) {
          reject(err);
        }
      });
      this.run();
    });
  }

  private run() {
    while (this.running < this.concurrency && this.queue.length > 0) {
      const task = this.queue.shift();
      if (!task) {
        return;
      }

      this.running++;

      task().finally(() => {
        this.running--;
        this.run();
      });
    }
  }
}
