import { Email, endpoint } from '../../../../src';

export default endpoint()
  .body({ name: String, email: Email })
  .returns({ id: String, name: String, email: String })
  .handle(async (ctx) => {
    const b = await ctx.body();
    return { id: '1', name: b.name, email: b.email };
  });
