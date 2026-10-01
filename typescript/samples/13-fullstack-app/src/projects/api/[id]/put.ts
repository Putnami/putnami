import { endpoint } from '@putnami/application';
import { ProjectService } from '../../project.service';

const allowedStatus = new Set(['active', 'completed', 'archived']);

export const PUT = endpoint()
  .params({ id: String })
  .body({ status: String })
  .inject({ projectService: ProjectService })
  .handle(async (ctx) => {
    const body = await ctx.body();
    if (!allowedStatus.has(body.status)) {
      return new Response('Invalid status', { status: 400 });
    }

    const project = await ctx.deps.projectService.setStatus(ctx.params.id, body.status, 'api.manual');
    if (!project) {
      return new Response('Project not found', { status: 404 });
    }

    return { project };
  });
