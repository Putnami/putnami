import { endpoint } from '@putnami/application';
import { Optional, Stream } from '@putnami/runtime';
import { getActivity, subscribeActivity } from '../../../shared/activity.feed';

export const GET = endpoint()
  .query({ replay: Optional(Number) })
  .returns(
    Stream({
      id: String,
      module: String,
      type: String,
      message: String,
      projectId: Optional(String),
      taskId: Optional(String),
      timestamp: Number,
    }),
  )
  .handle(async (ctx) => {
    const query = ctx.queryParams();
    const replay = query.replay ?? 20;
    for (const event of getActivity('projects', replay)) {
      ctx.send({
        ...event,
        projectId: event.projectId ?? undefined,
        taskId: event.taskId ?? undefined,
      });
    }

    const unsubscribe = subscribeActivity('projects', (event) =>
      ctx.send({
        ...event,
        projectId: event.projectId ?? undefined,
        taskId: event.taskId ?? undefined,
      }),
    );

    await new Promise<void>((resolve) => {
      ctx.req.signal.addEventListener(
        'abort',
        () => {
          unsubscribe();
          resolve();
        },
        { once: true },
      );
    });
  });
