import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { Repository } from '@putnami/database';
import { Users } from '../../tables/users';

// GET /users — List users with optional filtering and pagination
export const GET = endpoint()
  .query({
    limit: Optional(Number),
    offset: Optional(Number),
    orderBy: Optional(String),
  })
  .handle(async (ctx) => {
    const query = ctx.queryParams();
    const repo = new Repository(Users);

    const users = await repo.find(
      {},
      {
        limit: query.limit || 50,
        offset: query.offset || 0,
        orderBy: query.orderBy || 'createdAt',
      },
    );

    const total = await repo.count({});

    return { users, total, limit: query.limit || 50, offset: query.offset || 0 };
  });
