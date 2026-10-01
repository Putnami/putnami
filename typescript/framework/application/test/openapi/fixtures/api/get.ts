import { endpoint, Optional } from '../../../../src';

export default endpoint()
  .query({ page: Optional(Number), limit: Optional(Number) })
  .returns({ users: String, page: Number })
  .handle((ctx) => {
    const q = ctx.queryParams();
    return { users: '[]', page: q.page ?? 1 };
  });
