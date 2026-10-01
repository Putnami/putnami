import { runInContext } from '@putnami/runtime';
import type { HttpRequestContext } from './http-context.type';

/** Transfer request-scope ownership to the raw response reader. */
export function retainRawHttpStreamScope(
  response: Response,
  context: HttpRequestContext,
  close: () => void | Promise<void>,
): Response {
  const source = response.body;
  if (!source) throw new Error('Raw HTTP response scope requires a body');
  const reader = source.getReader();
  let completion: Promise<void> | undefined;
  const finish = (): Promise<void> => {
    context.req.signal.removeEventListener('abort', abort);
    completion ??= Promise.resolve().then(close);
    return completion;
  };
  const abort = (): void => {
    void reader
      .cancel(context.req.signal.reason)
      .catch(() => {})
      .finally(finish);
  };
  context.req.signal.addEventListener('abort', abort, { once: true });
  if (context.req.signal.aborted) abort();
  const body = new ReadableStream<Uint8Array>(
    {
      async pull(controller) {
        try {
          const item = await runInContext(context, () => reader.read());
          if (item.done) {
            controller.close();
            await finish();
          } else controller.enqueue(item.value);
        } catch (error) {
          controller.error(error);
          await reader.cancel(error).catch(() => {});
          await finish();
        }
      },
      async cancel(reason) {
        try {
          await reader.cancel(reason);
        } finally {
          await finish();
        }
      },
    },
    { highWaterMark: 0 },
  );
  return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers });
}
