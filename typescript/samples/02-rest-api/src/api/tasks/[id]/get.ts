import { endpoint } from '@putnami/application';
import { tasks } from '../../../store';

// GET /tasks/[id] — Get a single task
export const GET = endpoint()
  .params({ id: String })
  .handle(async (ctx) => {
    const task = tasks.get(ctx.params.id);

    if (!task) {
      return new Response('Task not found', { status: 404 });
    }

    return { task };
  });
