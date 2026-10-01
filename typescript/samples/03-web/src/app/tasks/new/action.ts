import { action } from '@putnami/web';
import type { Task } from '../../../store';
import { tasks } from '../../../store';

export default action()
  .body({ title: String })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const id = crypto.randomUUID();
    const now = new Date().toISOString();

    const task: Task = {
      id,
      title: body.title,
      completed: false,
      createdAt: now,
    };

    tasks.set(id, task);

    return { success: true };
  });
