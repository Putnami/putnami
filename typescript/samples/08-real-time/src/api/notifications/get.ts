import { endpoint } from '@putnami/application';
import { Stream } from '@putnami/runtime';

// GET /notifications — Server-Sent Events stream
export default endpoint()
  .returns(
    Stream({
      id: Number,
      message: String,
      timestamp: Number,
    }),
  )
  .handle(async (ctx) => {
    const signal = (ctx as unknown as { readonly signal: AbortSignal }).signal;
    if (signal.aborted) return;

    ctx.send({ id: 0, message: 'Connected', timestamp: Date.now() });

    let count = 0;
    const interval = setInterval(() => {
      count++;
      ctx.send({
        id: count,
        message: `Notification #${count}`,
        timestamp: Date.now(),
      });
    }, 3000);

    // Keep the handler alive until the underlying request is canceled.
    await new Promise<void>((resolve) => {
      const stop = () => {
        clearInterval(interval);
        resolve();
      };

      signal.addEventListener('abort', stop, { once: true });
    });
  });
