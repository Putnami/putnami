import { loader } from '@putnami/web';
import { ProjectService } from '../../projects/project.service';
import { TaskService } from '../task.service';

export default loader()
  .inject({ taskService: TaskService, projectService: ProjectService })
  .handle(async ({ taskService, projectService }) => {
    const [tasks, projects] = await Promise.all([taskService.listRecentTasks(100), projectService.listProjects()]);
    const projectNames = Object.fromEntries(projects.map((project) => [project.id, project.name]));

    return { tasks, projectNames };
  });
