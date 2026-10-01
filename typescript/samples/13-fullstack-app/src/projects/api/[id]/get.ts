import { endpoint } from '@putnami/application';
import { ProjectService } from '../../project.service';

// GET /projects/[id] — Get project with tasks
export const GET = endpoint()
  .params({ id: String })
  .inject({ projectService: ProjectService })
  .handle(async (ctx) => {
    const project = await ctx.deps.projectService.getProject(ctx.params.id);

    if (!project) {
      return new Response('Project not found', { status: 404 });
    }

    return { project };
  });
