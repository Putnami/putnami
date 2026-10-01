import { describe, expect, it } from 'bun:test';
import { MessageStream } from '../../../src/api/stream/message-stream';

describe('MessageStream', () => {
  it('should yield pushed messages in order', async () => {
    const stream = new MessageStream<string>();
    stream.push('a');
    stream.push('b');
    stream.push('c');
    stream.close();

    const results: string[] = [];
    for await (const msg of stream) {
      results.push(msg);
    }
    expect(results).toEqual(['a', 'b', 'c']);
  });

  it('should bound the buffer and drop messages past the queue cap (DoS guard)', async () => {
    const stream = new MessageStream<number>(3);
    // No consumer pulling: 5 pushes against a cap of 3 → 2 dropped.
    for (let i = 0; i < 5; i++) stream.push(i);
    expect(stream.droppedCount).toBe(2);
    stream.close();

    const results: number[] = [];
    for await (const msg of stream) {
      results.push(msg);
    }
    // Only the first 3 (within the bound) are retained.
    expect(results).toEqual([0, 1, 2]);
  });

  it('should wait for messages when queue is empty', async () => {
    const stream = new MessageStream<number>();
    const results: number[] = [];

    const consuming = (async () => {
      for await (const msg of stream) {
        results.push(msg);
      }
    })();

    // Push after consumer is waiting
    await Promise.resolve(); // yield to let for-await start
    stream.push(1);
    stream.push(2);
    stream.close();

    await consuming;
    expect(results).toEqual([1, 2]);
  });

  it('should end iteration when closed with no pending messages', async () => {
    const stream = new MessageStream<string>();
    stream.close();

    const results: string[] = [];
    for await (const msg of stream) {
      results.push(msg);
    }
    expect(results).toEqual([]);
  });

  it('should ignore pushes after close', async () => {
    const stream = new MessageStream<string>();
    stream.push('before');
    stream.close();
    stream.push('after');

    const results: string[] = [];
    for await (const msg of stream) {
      results.push(msg);
    }
    expect(results).toEqual(['before']);
  });

  it('should resolve waiting consumer on close', async () => {
    const stream = new MessageStream<string>();

    const iterator = stream[Symbol.asyncIterator]();
    const nextPromise = iterator.next();

    // Close while consumer is waiting
    stream.close();

    const result = await nextPromise;
    expect(result.done).toBe(true);
  });

  it('should handle interleaved push and consume', async () => {
    const stream = new MessageStream<number>();
    const results: number[] = [];

    const consuming = (async () => {
      for await (const msg of stream) {
        results.push(msg);
      }
    })();

    for (let i = 0; i < 5; i++) {
      await Promise.resolve();
      stream.push(i);
    }
    stream.close();

    await consuming;
    expect(results).toEqual([0, 1, 2, 3, 4]);
  });
});
