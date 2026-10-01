import { action } from '@putnami/web';
import { addEntry } from './loader';

export default action()
  .body({ name: String, message: String })
  .handle(async (ctx) => {
    const { name, message } = await ctx.body();
    const entry = addEntry(name, message);
    return { ok: true, entry };
  });
