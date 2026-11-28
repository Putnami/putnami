import { afterAll, beforeAll, describe, expect, it, type Mock, spyOn } from 'bun:test';
import { BadRequestException, Default, Int, MaxLength, Optional, type SchemaDefinition, Uuid } from '@putnami/runtime';
import { api } from '../../../src/api/api.plugin';
import { endpoint } from '../../../src/api/route/endpoint';
import * as genericValidation from '../../../src/api/route/validate';
import { validateSchema } from '../../../src/api/route/validate';
import type { Application } from '../../../src/application';
import { application } from '../../../src/application';
import { http } from '../../../src/http/http.plugin';

const paramsSchema = { id: Uuid };
const querySchema = { page: Default(Int, 1), q: Optional(String) };
const bodySchema = { title: MaxLength(200), description: Default(String, ''), priority: Default(Int, 1) };

/** The body the generic validator answers for an input it rejects. */
function genericRejection(schema: SchemaDefinition, value: unknown, coerce: boolean, label: string): string {
  try {
    validateSchema(schema, value, { coerce, label });
  } catch (error) {
    expect(error).toBeInstanceOf(BadRequestException);
    return JSON.stringify((error as { getResponse(): unknown }).getResponse());
  }
  throw new Error('the generic validator accepted the input');
}

describe('endpoint input validation compiled per endpoint', () => {
  let app: Application;
  let baseUrl: string;
  // The generic validator, watched to tell which inputs reach it; it still runs.
  let generic: Mock<typeof validateSchema>;

  beforeAll(async () => {
    generic = spyOn(genericValidation, 'validateSchema');
    const httpPlugin = http({ port: 0 });
    const routes = api({ autoScan: false })
      .register(
        '/tasks/[id]',
        {
          default: endpoint()
            .params(paramsSchema)
            .query(querySchema)
            .handle((ctx) => ({ params: ctx.params, query: ctx.queryParams() })),
        },
        'GET',
      )
      .register(
        '/tasks',
        {
          default: endpoint()
            .body(bodySchema)
            .handle(async (ctx) => ({ body: await ctx.body() })),
        },
        'POST',
      );
    app = application().use(httpPlugin).use(routes);
    await app.start();
    baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
  });

  afterAll(async () => {
    await app.stop();
    generic.mockRestore();
  });

  /** The response to a request, and whether the generic validator ran for it. */
  async function answer(request: () => Promise<Response>): Promise<{ res: Response; generic: boolean }> {
    generic.mockClear();
    const res = await request();
    return { res, generic: generic.mock.calls.length > 0 };
  }

  const id = '550e8400-e29b-41d4-a716-446655440000';
  const post = (body: unknown) =>
    fetch(`${baseUrl}/tasks`, {
      method: 'POST',
      headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });

  it('hands the handler the params and query it accepts, coerced and with their defaults', async () => {
    const accepted = await answer(() => fetch(`${baseUrl}/tasks/${id}?page=3&q=report`));
    expect(accepted.res.status).toBe(200);
    expect(await accepted.res.json()).toEqual({ params: { id }, query: { page: 3, q: 'report' } });
    expect(accepted.generic).toBe(false);

    const defaults = await answer(() => fetch(`${baseUrl}/tasks/${id}`));
    expect(await defaults.res.json()).toEqual({ params: { id }, query: { page: 1 } });
    expect(defaults.generic).toBe(false);
  });

  it('answers a rejected param or query with the 400 the generic validator answers', async () => {
    const badId = await answer(() => fetch(`${baseUrl}/tasks/not-a-uuid`));
    expect(badId.res.status).toBe(400);
    expect(badId.generic).toBe(true);
    expect(await badId.res.text()).toBe(genericRejection(paramsSchema, { id: 'not-a-uuid' }, true, 'params'));

    const badPage = await answer(() => fetch(`${baseUrl}/tasks/${id}?page=1.5`));
    expect(badPage.res.status).toBe(400);
    expect(badPage.generic).toBe(true);
    expect(await badPage.res.text()).toBe(genericRejection(querySchema, { page: '1.5' }, true, 'query'));
  });

  it('hands the handler the body it accepts, with its defaults', async () => {
    const accepted = await answer(() => post({ title: 'Write the report' }));
    expect(accepted.res.status).toBe(200);
    expect(await accepted.res.json()).toEqual({ body: { title: 'Write the report', description: '', priority: 1 } });
    expect(accepted.generic).toBe(false);
  });

  it('answers a rejected body with the 400 the generic validator answers', async () => {
    const bodies = [{ title: 'x'.repeat(201), priority: 1.5 }, { description: 'no title' }, { title: 7 }];
    const answers = [];
    for (const body of bodies) {
      const { res, generic: reached } = await answer(() => post(body));
      answers.push({ status: res.status, text: await res.text(), generic: reached });
    }
    expect(answers).toEqual(
      bodies.map((body) => ({ status: 400, text: genericRejection(bodySchema, body, false, 'body'), generic: true })),
    );
  });
});
