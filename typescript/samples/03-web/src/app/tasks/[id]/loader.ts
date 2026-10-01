import { loader } from '@putnami/web';
import { tasks } from '../../../store';

export default loader()
  .params({ id: String })
  .handle(async (ctx) => {
    const task = tasks.get(ctx.params.id);

    if (!task) {
      return new Response('Task not found', { status: 404 });
    }

    return { task };
  });
