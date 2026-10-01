import { endpoint } from '@putnami/application';

export default endpoint()
  .body({ name: String })
  .handle(async (ctx) => {
    const { name } = await ctx.body();
    return { message: `Created: ${name}` };
  });
