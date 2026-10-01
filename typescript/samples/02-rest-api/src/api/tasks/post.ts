import { endpoint, json } from '@putnami/application';
import { Default, Int, MaxLength } from '@putnami/runtime';
import type { Task } from '../../store';
import { tasks } from '../../store';

// POST /tasks — Create a new task
export const POST = endpoint()
  .body({
    title: MaxLength(200),
    description: Default(String, ''),
    priority: Default(Int, 1),
  })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const id = crypto.randomUUID();
    const now = new Date().toISOString();

    const task: Task = {
      id,
      title: body.title,
      description: body.description,
      status: 'todo',
      priority: body.priority,
      createdAt: now,
      updatedAt: now,
    };

    tasks.set(id, task);

    return json({ task }, { status: 201 });
  });
