import { endpoint } from '@putnami/application';
import { OneOf, Optional } from '@putnami/runtime';
import { tasks } from '../../../store';

// PUT /tasks/[id] — Update a task
export const PUT = endpoint()
  .params({ id: String })
  .body({
    title: Optional(String),
    description: Optional(String),
    status: Optional(OneOf('todo', 'in_progress', 'done')),
    priority: Optional(Number),
  })
  .handle(async (ctx) => {
    const task = tasks.get(ctx.params.id);

    if (!task) {
      return new Response('Task not found', { status: 404 });
    }

    const body = await ctx.body();
    const now = new Date().toISOString();

    if (body.title !== undefined) task.title = body.title;
    if (body.description !== undefined) task.description = body.description;
    if (body.status !== undefined) task.status = body.status;
    if (body.priority !== undefined) task.priority = body.priority;
    task.updatedAt = now;

    tasks.set(ctx.params.id, task);

    return { task };
  });
