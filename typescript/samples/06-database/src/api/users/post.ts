import { endpoint, json } from '@putnami/application';
import { Email, Int } from '@putnami/runtime';
import { Repository } from '@putnami/database';
import { Users } from '../../tables/users';

// POST /users — Create a new user
export const POST = endpoint()
  .body({
    email: Email,
    name: String,
    age: Int,
  })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const repo = new Repository(Users);

    const user = await repo.save({
      id: crypto.randomUUID(),
      email: body.email,
      name: body.name,
      age: body.age,
    });

    return json({ user }, { status: 201 });
  });
