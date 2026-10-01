import { action } from '@putnami/web';
import { TaskService } from '../task.service';

const nextStatus: Record<string, string> = { todo: 'in_progress', in_progress: 'done', done: 'todo' };

export default action()
  .body({ taskId: String })
  .inject({ taskService: TaskService })
  .handle(async ({ taskService }, ctx) => {
    const body = await ctx.body();
    const task = await taskService.getTask(body.taskId);
    if (!task) return new Response('Task not found', { status: 404 });

    const updated = await taskService.updateTask(body.taskId, {
      status: nextStatus[task.status],
    });

    return { task: updated };
  });
