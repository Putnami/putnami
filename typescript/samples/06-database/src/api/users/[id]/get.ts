import { endpoint, notFound } from '@putnami/application';
import { Uuid } from '@putnami/runtime';
import { Repository } from '@putnami/database';
import { Users } from '../../../tables/users';

// GET /users/[id] — Get a single user
export const GET = endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    const repo = new Repository(Users);
    const user = await repo.get({ id: ctx.params.id });

    if (!user) {
      return notFound({ error: 'User not found' });
    }

    return { user };
  });
