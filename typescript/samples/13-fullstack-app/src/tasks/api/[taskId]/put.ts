import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { TaskService } from '../../task.service';
import type { Task } from '../../task.service';

// PUT /tasks/[taskId] — Update a task
export const PUT = endpoint()
  .params({ taskId: String })
  .body({
    title: Optional(String),
    status: Optional(String),
    priority: Optional(Number),
    assignee: Optional(String),
  })
  .inject({ taskService: TaskService })
  .handle(async (ctx) => {
    const { taskService } = ctx.deps;
    const body = await ctx.body();
    const updates: Partial<Pick<Task, 'title' | 'status' | 'priority' | 'assignee'>> = {};
    if (body.title !== undefined) updates.title = body.title;
    if (body.priority !== undefined) updates.priority = body.priority;
    if (body.assignee !== undefined) updates.assignee = body.assignee;
    if (body.status === 'todo' || body.status === 'in_progress' || body.status === 'done') {
      updates.status = body.status;
    }

    const task = await taskService.updateTask(ctx.params.taskId, updates);

    if (!task) {
      return new Response('Task not found', { status: 404 });
    }

    return { task };
  });
