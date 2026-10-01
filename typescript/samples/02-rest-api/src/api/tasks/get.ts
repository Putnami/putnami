import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { tasks } from '../../store';

// GET /tasks — List all tasks with optional status filter
export const GET = endpoint()
  .query({ status: Optional(String) })
  .handle(async (ctx) => {
    const query = ctx.queryParams();
    let items = [...tasks.values()];

    if (query.status) {
      items = items.filter((t) => t.status === query.status);
    }

    items.sort((a, b) => a.priority - b.priority);

    return { tasks: items, total: items.length };
  });
