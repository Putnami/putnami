import { endpoint } from '@putnami/application';
import { Optional, Uuid } from '@putnami/runtime';
import { Repository } from '@putnami/database';
import { Users } from '../../../tables/users';

// PUT /users/[id] — Update a user
export const PUT = endpoint()
  .params({ id: Uuid })
  .body({
    email: Optional(String),
    name: Optional(String),
    age: Optional(Number),
  })
  .handle(async (ctx) => {
    const repo = new Repository(Users);
    const existing = await repo.get({ id: ctx.params.id });

    if (!existing) {
      return new Response('User not found', { status: 404 });
    }

    const body = await ctx.body();
    const user = await repo.save({
      ...existing,
      ...body,
      id: ctx.params.id,
    });

    return { user };
  });
