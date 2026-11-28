import { endpoint, json } from '@putnami/application';
import { Default, Int, MaxLength } from '@putnami/runtime';
import { insertTask } from '../../data';

// Title rule aligned across subjects: at most 200 UTF-16 units (MaxLength,
// as TypeBox's maxLength counts).
export const POST = endpoint()
  .body({
    title: MaxLength(200),
    description: Default(String, ''),
    priority: Default(Int, 1),
  })
  .handle(async (ctx) => {
    const body = await ctx.body();
    const task = await insertTask(body.title, body.description, body.priority);
    return json({ task }, { status: 201 });
  });
