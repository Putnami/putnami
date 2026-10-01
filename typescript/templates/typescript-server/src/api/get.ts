import { endpoint, Optional } from '@putnami/application';

export default endpoint()
  .query({ name: Optional(String) })
  .handle((ctx) => {
    const { name } = ctx.queryParams();
    return { message: `Hello, ${name ?? 'World'}!` };
  });
