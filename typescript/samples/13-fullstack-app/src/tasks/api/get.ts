import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { TaskService } from '../task.service';

export const GET = endpoint()
  .query({ projectId: Optional(String), limit: Optional(Number) })
  .inject({ taskService: TaskService })
  .handle(async (ctx) => {
    const { taskService } = ctx.deps;
    const query = ctx.queryParams();

    if (query.projectId) {
      return {
        tasks: await taskService.listTasks(query.projectId),
      };
    }

    return {
      tasks: await taskService.listRecentTasks(query.limit ?? 100),
    };
  });
