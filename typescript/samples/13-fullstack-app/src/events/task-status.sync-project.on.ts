import { handler } from '@putnami/events';
import { ProjectService } from '../projects/project.service';
import { TaskStatusChanged } from '../tasks/tasks.topics';

export default handler(TaskStatusChanged)
  .inject({ projectService: ProjectService })
  .handle(async ({ projectService }, msg) => {
    const nextStatus = msg.payload.totalTasks > 0 && msg.payload.remainingOpenTasks === 0 ? 'completed' : 'active';

    await projectService.setStatus(msg.payload.projectId, nextStatus, 'tasks.progress');
  });
