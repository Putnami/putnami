import { track } from '@putnami/analytics';
import { endpoint, type HttpRequestContext, json } from '@putnami/application';
import { Default, Int, useContext } from '@putnami/runtime';
import { TaskService } from '../task.service';

// POST /tasks — Create a task in a project
export const POST = endpoint()
  .body({
    projectId: String,
    title: String,
    description: Default(String, ''),
    priority: Default(Int, 1),
    assignee: Default(String, ''),
  })
  .inject({ taskService: TaskService })
  .handle(async (ctx) => {
    const { taskService } = ctx.deps;
    const body = await ctx.body();
    const task = await taskService.createTask(body.projectId, {
      title: body.title,
      description: body.description,
      priority: body.priority,
      assignee: body.assignee,
    });
    // The handler tracks, not the service: `track()` needs the request context
    // and only the HTTP layer has one. An endpoint handler narrows its own
    // `ctx`, so the full request context is read from the async context — the
    // same call the web action handler makes.
    await track(useContext<HttpRequestContext>(), 'task_created', { priority: body.priority });
    return json({ task }, { status: 201 });
  });
