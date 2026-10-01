import { endpoint } from '@putnami/application';
import { TaskService } from '../../task.service';

export const GET = endpoint()
  .params({ taskId: String })
  .inject({ taskService: TaskService })
  .handle(async (ctx) => {
    const task = await ctx.deps.taskService.getTask(ctx.params.taskId);
    if (!task) {
      return new Response('Task not found', { status: 404 });
    }

    return { task };
  });
