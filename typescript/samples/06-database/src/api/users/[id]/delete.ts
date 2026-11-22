import { endpoint, notFound } from '@putnami/application';
import { Uuid } from '@putnami/runtime';
import { Repository } from '@putnami/database';
import { Users } from '../../../tables/users';

// DELETE /users/[id] — Delete a user
export const DELETE = endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    const repo = new Repository(Users);
    const existing = await repo.get({ id: ctx.params.id });

    if (!existing) {
      return notFound({ error: 'User not found' });
    }

    await repo.delete({ id: ctx.params.id });

    return { deleted: true, id: ctx.params.id };
  });
