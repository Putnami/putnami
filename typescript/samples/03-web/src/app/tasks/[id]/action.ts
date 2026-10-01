import { action } from '@putnami/web';
import { tasks } from '../../../store';

export default action()
  .params({ id: String })
  .body({ completed: String })
  .handle(async (ctx) => {
    const task = tasks.get(ctx.params.id);

    if (!task) {
      return new Response('Task not found', { status: 404 });
    }

    const body = await ctx.body();
    task.completed = body.completed === 'true';
    tasks.set(ctx.params.id, task);

    return { success: true };
  });
