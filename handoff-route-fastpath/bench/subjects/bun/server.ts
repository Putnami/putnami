// Plain Bun.serve subject with Putnami's default protections, hand-written.
import { getTask, insertTask, listTasks } from '../common/db';
import {
  badRequest, bodyTooLarge, head, json, MAX_BODY, methodNotAllowed, notFound, options,
  originGuard, TITLE_MAX, tooLarge, UUID_RE, withDeadline,
} from '../common/protect';

const health = () => json({ status: 'ok' });

async function getOne(req: Request & { params: { id: string } }): Promise<Response> {
  const id = req.params.id;
  if (!UUID_RE.test(id)) return badRequest('params.id must be a valid UUID');
  const task = await getTask(id);
  return task ? json({ task }) : json({ error: 'Task not found' }, 404);
}

async function list(req: Request): Promise<Response> {
  const q = new URL(req.url).searchParams.get('limit');
  let n = 20;
  if (q !== null) {
    n = Number(q);
    if (Number.isNaN(n)) return badRequest('query.limit must be a number');
  }
  const limit = Math.min(Math.max(Math.floor(n || 20), 1), 100);
  return json({ tasks: await listTasks(limit) });
}

async function create(req: Request): Promise<Response> {
  const blocked = originGuard(req);
  if (blocked) return blocked;
  if (bodyTooLarge(req)) return tooLarge();
  const text = await req.text();
  if (text.length > MAX_BODY) return tooLarge();
  let body: any;
  try {
    body = JSON.parse(text);
  } catch {
    return badRequest('Malformed JSON body');
  }
  if (body === null || typeof body !== 'object') return badRequest('body must be an object');
  const { title, description = '', priority = 1 } = body;
  if (typeof title !== 'string') return badRequest('body.title is required');
  if (title.length > TITLE_MAX) return badRequest(`body.title must have length <= ${TITLE_MAX}`);
  if (typeof description !== 'string') return badRequest('body.description must be a string');
  if (!Number.isInteger(priority)) return badRequest('body.priority must be an integer');
  return json({ task: await insertTask(title, description, priority) }, 201);
}

const GET_ONLY = 'GET, HEAD, OPTIONS';
const GET_POST = 'GET, HEAD, OPTIONS, POST';
const d = (fn: (req: any) => Response | Promise<Response>) => (req: Request) => withDeadline(fn(req));
const h = (fn: (req: any) => Response | Promise<Response>) => (req: Request) =>
  withDeadline(Promise.resolve(fn(req)).then(head));
const nope = (allow: string) => () => methodNotAllowed(allow);

const server = Bun.serve({
  port: Number(process.env.PORT ?? 3907),
  hostname: '0.0.0.0',
  maxRequestBodySize: MAX_BODY + 64 * 1024,
  idleTimeout: 40,
  routes: {
    '/health': {
      GET: d(health), HEAD: h(health), OPTIONS: () => options(GET_ONLY),
      POST: nope(GET_ONLY), PUT: nope(GET_ONLY), PATCH: nope(GET_ONLY), DELETE: nope(GET_ONLY),
    },
    '/tasks/:id': {
      GET: d(getOne), HEAD: h(getOne), OPTIONS: () => options(GET_ONLY),
      POST: nope(GET_ONLY), PUT: nope(GET_ONLY), PATCH: nope(GET_ONLY), DELETE: nope(GET_ONLY),
    },
    '/tasks': {
      GET: d(list), HEAD: h(list), POST: d(create), OPTIONS: () => options(GET_POST),
      PUT: nope(GET_POST), PATCH: nope(GET_POST), DELETE: nope(GET_POST),
    },
  },
  fetch: () => notFound(),
});
console.log(`bun subject listening on ${server.port}`);
