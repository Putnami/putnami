// Elysia subject with Putnami's default protections, hand-written.
import { Elysia, t } from 'elysia';
import { getTask, insertTask, listTasks } from '../common/db';
import {
  badRequest, bodyTooLarge, head, json, MAX_BODY, methodNotAllowed, notFound, options,
  originGuard, TITLE_MAX, tooLarge, withDeadline,
} from '../common/protect';

const GET_ONLY = 'GET, HEAD, OPTIONS';
const GET_POST = 'GET, HEAD, OPTIONS, POST';
const ALLOW: [RegExp, string][] = [
  [/^\/health$/, GET_ONLY],
  [/^\/tasks\/[^/]+$/, GET_ONLY],
  [/^\/tasks$/, GET_POST],
];

const health = () => json({ status: 'ok' });
const getOne = async (id: string) => {
  const task = await getTask(id);
  return task ? json({ task }) : json({ error: 'Task not found' }, 404);
};
const list = async (limit: number | undefined) =>
  json({ tasks: await listTasks(Math.min(Math.max(Math.floor(limit || 20), 1), 100)) });

const idParams = t.Object({ id: t.String({ format: 'uuid' }) });
const listQuery = t.Object({ limit: t.Optional(t.Numeric()) });

const app = new Elysia({ serve: { maxRequestBodySize: MAX_BODY + 64 * 1024, idleTimeout: 40 } })
  // The origin guard and the body limit answer an unsafe method before its body is parsed.
  .onRequest(({ request }) => {
    const m = request.method;
    if (m === 'GET' || m === 'HEAD' || m === 'OPTIONS') return;
    const blocked = originGuard(request);
    if (blocked) return blocked;
    if (bodyTooLarge(request)) return tooLarge();
  })
  .onError(({ code, error, request }) => {
    if (code === 'VALIDATION') {
      const where = (error as any).type === 'params' ? 'params.id must be a valid UUID' : 'Validation failed';
      return badRequest(where);
    }
    if (code === 'PARSE') return badRequest('Malformed JSON body');
    if (code === 'NOT_FOUND') {
      const path = new URL(request.url).pathname;
      for (const [re, allow] of ALLOW) if (re.test(path)) return methodNotAllowed(allow);
      return notFound();
    }
  })
  .get('/health', () => withDeadline(health()))
  .head('/health', () => withDeadline(head(health())))
  .options('/health', () => options(GET_ONLY))
  .get('/tasks/:id', ({ params }) => withDeadline(getOne(params.id)), { params: idParams })
  .head('/tasks/:id', ({ params }) => withDeadline(getOne(params.id).then(head)), { params: idParams })
  .options('/tasks/:id', () => options(GET_ONLY))
  .get('/tasks', ({ query }) => withDeadline(list(query.limit)), { query: listQuery })
  .head('/tasks', ({ query }) => withDeadline(list(query.limit).then(head)), { query: listQuery })
  .options('/tasks', () => options(GET_POST))
  .post(
    '/tasks',
    ({ body }) =>
      withDeadline(
        insertTask(body.title, body.description, body.priority).then((task) => json({ task }, 201)),
      ),
    {
      body: t.Object({
        title: t.String({ maxLength: TITLE_MAX }),
        description: t.String({ default: '' }),
        priority: t.Integer({ default: 1 }),
      }),
    },
  )
  .listen({ port: Number(process.env.PORT ?? 3908), hostname: '0.0.0.0' });

console.log(`elysia subject listening on ${app.server?.port}`);
