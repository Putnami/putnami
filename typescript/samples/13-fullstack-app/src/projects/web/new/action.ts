import { action } from '@putnami/web';
import { Optional } from '@putnami/runtime';
import { ProjectService } from '../../project.service';

export default action()
  .body({ name: String, description: Optional(String) })
  .inject({ projectService: ProjectService })
  .handle(async ({ projectService }, ctx) => {
    const body = await ctx.body();
    const project = await projectService.createProject({
      name: body.name,
      description: body.description || '',
    });

    return { project };
  });
