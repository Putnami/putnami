import { action } from '@putnami/web';
import { Optional } from '@putnami/runtime';
import { ProjectService } from '../../project.service';
import { TaskService } from '../../../tasks/task.service';

const nextStatus: Record<string, string> = { todo: 'in_progress', in_progress: 'done', done: 'todo' };

export default action()
  .params({ id: String })
  .body({ intent: String, title: Optional(String), taskId: Optional(String) })
  .inject({ taskService: TaskService, projectService: ProjectService })
  .handle(async ({ taskService, projectService }, ctx) => {
    const body = await ctx.body();

    if (body.intent === 'add-task') {
      const task = await taskService.createTask(ctx.params.id, {
        title: body.title!,
        description: '',
        priority: 1,
        assignee: '',
      });
      return { task };
    }

    if (body.intent === 'toggle-task') {
      const task = await taskService.getTask(body.taskId!);
      if (!task) return new Response('Task not found', { status: 404 });

      const updated = await taskService.updateTask(body.taskId!, {
        status: nextStatus[task.status],
      });
      return { task: updated };
    }

    if (body.intent === 'toggle-project-status') {
      const project = await projectService.getProject(ctx.params.id);
      if (!project) return new Response('Project not found', { status: 404 });

      const next = project.status === 'archived' ? 'active' : 'archived';
      const updated = await projectService.setStatus(ctx.params.id, next, 'web.toggle');
      return { project: updated };
    }

    return new Response('Unknown intent', { status: 400 });
  });
