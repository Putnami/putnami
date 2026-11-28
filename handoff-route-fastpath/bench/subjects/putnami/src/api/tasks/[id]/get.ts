import { endpoint, notFound } from '@putnami/application';
import { Uuid } from '@putnami/runtime';
import { getTask } from '../../../data';

export const GET = endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    const task = await getTask(ctx.params.id);
    if (!task) return notFound({ error: 'Task not found' });
    return { task };
  });
