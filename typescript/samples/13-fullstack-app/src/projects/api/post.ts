import { endpoint, json } from '@putnami/application';
import { ProjectService } from '../project.service';

// POST /projects — Create a new project
export const POST = endpoint()
  .body({ name: String, description: String })
  .inject({ projectService: ProjectService })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const project = await ctx.deps.projectService.createProject(body);
    return json({ project }, { status: 201 });
  });
