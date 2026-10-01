import { handler } from '@putnami/events';
import { ProjectCreated } from '../projects/projects.topics';
import { TaskService } from '../tasks/task.service';

export default handler(ProjectCreated)
  .inject({ taskService: TaskService })
  .handle(async ({ taskService }, msg) => {
    await taskService.ensureSystemTask(msg.payload.projectId, 'Kickoff: define success criteria');
  });
