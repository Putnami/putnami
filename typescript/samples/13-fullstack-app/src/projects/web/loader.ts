import { loader } from '@putnami/web';
import { ProjectService } from '../project.service';

export default loader()
  .inject({ projectService: ProjectService })
  .handle(async ({ projectService }) => ({ projects: await projectService.listProjects() }));
