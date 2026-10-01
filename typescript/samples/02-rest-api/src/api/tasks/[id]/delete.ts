import { endpoint } from '@putnami/application';
import { tasks } from '../../../store';

// DELETE /tasks/[id] — Delete a task
export const DELETE = endpoint()
  .params({ id: String })
  .handle(async (ctx) => {
    const task = tasks.get(ctx.params.id);

    if (!task) {
      return new Response('Task not found', { status: 404 });
    }

    tasks.delete(ctx.params.id);

    return { deleted: true, id: ctx.params.id };
  });
