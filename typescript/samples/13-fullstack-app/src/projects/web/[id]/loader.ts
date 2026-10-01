import { loader } from '@putnami/web';
import { ProjectService } from '../../project.service';
import { TaskService } from '../../../tasks/task.service';

export default loader()
  .params({ id: String })
  .inject({ projectService: ProjectService, taskService: TaskService })
  .handle(async ({ projectService, taskService }, ctx) => {
    const project = await projectService.getProject(ctx.params.id);

    if (!project) {
      return new Response('Project not found', { status: 404 });
    }

    const tasks = await taskService.listTasks(ctx.params.id);

    return { project, tasks };
  });
