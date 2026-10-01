import { endpoint } from '@putnami/application';
import { ProjectService } from '../project.service';

// GET /projects — List all projects
export const GET = endpoint()
  .inject({ projectService: ProjectService })
  .handle(async (ctx) => ({ projects: await ctx.deps.projectService.listProjects() }));
