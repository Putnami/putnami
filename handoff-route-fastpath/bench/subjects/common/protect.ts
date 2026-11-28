// Hand-written protections of the Elysia and plain-Bun subjects, the same as
// Putnami's defaults: security headers, origin guard, 1 MiB body limit, a 30 s
// request deadline, automatic HEAD and OPTIONS, and 405 with Allow.

export const MAX_BODY = 1024 * 1024;
export const DEADLINE_MS = 30_000;

const FULL_CSP =
  "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'";

/** Headers of a JSON answer (Putnami's JSON header set). */
export const JSON_HEADERS: Record<string, string> = {
  'Content-Type': 'application/json',
  'Content-Security-Policy': "frame-ancestors 'none'",
  'X-Content-Type-Options': 'nosniff',
};

/** Headers of a non-JSON answer (Putnami's full header set). */
export const FULL_HEADERS: Record<string, string> = {
  'Content-Security-Policy': FULL_CSP,
  'X-Content-Type-Options': 'nosniff',
  'X-Frame-Options': 'DENY',
  'Referrer-Policy': 'strict-origin-when-cross-origin',
  'Permissions-Policy': 'camera=(), microphone=(), geolocation=(), payment=()',
  'X-XSS-Protection': '0',
};

const PLAIN_JSON = { 'Content-Type': 'application/json' };

export const json = (body: unknown, status = 200): Response =>
  new Response(JSON.stringify(body), { status, headers: JSON_HEADERS });

export const options = (allow: string): Response =>
  new Response(null, { status: 204, headers: { ...FULL_HEADERS, Allow: allow } });

export const methodNotAllowed = (allow: string): Response =>
  new Response('{"error":"Method Not Allowed","statusCode":405}', {
    status: 405,
    headers: { ...PLAIN_JSON, Allow: allow },
  });

export const notFound = (): Response =>
  new Response('{"error":"Not Found","statusCode":404}', { status: 404, headers: PLAIN_JSON });

export const badRequest = (message: string): Response =>
  json({ statusCode: 400, message, error: 'Bad Request' }, 400);

export const tooLarge = (): Response => json(`Payload Too Large: body exceeds ${MAX_BODY} bytes`, 413);

export const head = (res: Response): Response => new Response(null, { status: res.status, headers: res.headers });

function hostOf(origin: string): string | undefined {
  const i = origin.indexOf('//');
  if (i === -1) return undefined;
  const rest = origin.slice(i + 2);
  const j = rest.indexOf('/');
  return j === -1 ? rest : rest.slice(0, j);
}

/** Putnami's origin guard for an unsafe method: undefined when allowed. */
export function originGuard(req: Request): Response | undefined {
  const site = req.headers.get('Sec-Fetch-Site');
  if (site) {
    if (site === 'same-origin' || site === 'none' || site === 'same-site') return undefined;
    return json({ error: 'Cross-origin request blocked' }, 403);
  }
  const origin = req.headers.get('Origin');
  if (!origin) return undefined;
  if (hostOf(origin) === req.headers.get('Host')) return undefined;
  return json({ error: 'Cross-origin request blocked' }, 403);
}

/** The body limit on the declared length; Bun.serve's maxRequestBodySize backs it for a chunked body. */
export function bodyTooLarge(req: Request): boolean {
  const len = req.headers.get('Content-Length');
  return len !== null && Number(len) > MAX_BODY;
}

// One sweeper for the deadlines of every request in flight, as Putnami does.
interface Inflight {
  at: number;
  expire: () => void;
}
const inflight = new Set<Inflight>();
setInterval(() => {
  const now = Date.now();
  for (const e of inflight) {
    if (e.at <= now) {
      inflight.delete(e);
      e.expire();
    }
  }
}, 1000).unref();

const TIMEOUT_BODY = '{"error":"Request Timeout","statusCode":408}';

/** Answer 408 when the work outlives the deadline. */
export function withDeadline(work: Response | Promise<Response>): Promise<Response> {
  return new Promise<Response>((resolve, reject) => {
    const e: Inflight = {
      at: Date.now() + DEADLINE_MS,
      expire: () => resolve(new Response(TIMEOUT_BODY, { status: 408, headers: JSON_HEADERS })),
    };
    inflight.add(e);
    Promise.resolve(work).then(
      (r) => {
        inflight.delete(e);
        resolve(r);
      },
      (err) => {
        inflight.delete(e);
        reject(err);
      },
    );
  });
}

// Aligned with the Putnami subject: the same queries, toTask and title rule.
export function toTask(r: any) {
  return {
    id: r.id,
    title: r.title,
    description: r.description,
    done: r.done,
    priority: r.priority,
    createdAt: r.created_at.toISOString(),
  };
}

export const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
export const TITLE_MAX = 200; // UTF-16 units, as TypeBox and Putnami's MaxLength count

// Exit cleanly on SIGTERM, so `bun --cpu-prof` writes its profile (as Putnami's own shutdown does).
process.on('SIGTERM', () => process.exit(0));
