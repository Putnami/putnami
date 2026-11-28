import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { listTasks } from '../../data';

export const GET = endpoint()
  .query({ limit: Optional(Number) })
  .handle(async (ctx) => {
    const q = ctx.queryParams();
    const limit = Math.min(Math.max(Math.floor(q.limit || 20), 1), 100);
    return { tasks: await listTasks(limit) };
  });
