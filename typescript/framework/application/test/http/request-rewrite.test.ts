import { afterEach, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { type Application, application } from '../../src/application';
import { HttpPlugin } from '../../src/http/http.plugin';
import { HttpResponse } from '../../src/http/http-response';

const feature = 'typescript/http-services';
const requirement = 'request-rewrite';

/** Prefix every path of a request to `fr.localhost` with `/fr`. */
const frenchHost = (req: Request, path: string) =>
  req.headers.get('host')?.startsWith('fr.localhost') ? `/fr${path}` : undefined;

describe('request rewrite', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  async function serve(root: HttpPlugin): Promise<(path: string, host?: string) => Promise<Response>> {
    app = application().use(root);
    await app.start();
    const port = root.getServer()?.port;
    return (path, host) =>
      fetch(`http://localhost:${port}${path}`, host ? { headers: { host: `${host}:${port}` } } : undefined);
  }

  const echo = (ctx: { route?: string; url: string; req: Request; queryParams: () => Record<string, string> }) =>
    HttpResponse.json({
      route: ctx.route,
      url: new URL(ctx.url).pathname,
      client: new URL(ctx.req.url).pathname,
      q: ctx.queryParams()['q'],
    });

  specTest(
    'matches and handles the rewritten path, and keeps the client URL on the request',
    { feature, requirement, check: 'a-rewritten-path-is-matched-and-handled' },
    async () => {
      const root = new HttpPlugin({ port: 0 })
        .get('/fr/pricing', echo)
        .get('/pricing', () => new HttpResponse('en'))
        .rewrite(frenchHost)
        .rewrite(() => '/pricing');
      const get = await serve(root);
      const french = await get('/pricing?q=1', 'fr.localhost');
      expect(await french.json()).toEqual({ route: '/fr/pricing', url: '/fr/pricing', client: '/pricing', q: '1' });
      // The first rewrite whose path is served wins, in registration order.
      expect(await (await get('/fr/pricing', 'fr.localhost')).text()).toBe('en');
      expect(await (await get('/pricing')).text()).toBe('en');
    },
  );

  specTest(
    'ignores a rewrite to a path no route serves',
    { feature, requirement, check: 'a-rewrite-to-a-path-no-route-serves-is-ignored' },
    async () => {
      const root = new HttpPlugin({ port: 0 }).get('/docs', () => new HttpResponse('docs')).rewrite(frenchHost);
      const get = await serve(root);
      const docs = await get('/docs', 'fr.localhost');
      expect(docs.status).toBe(200);
      expect(await docs.text()).toBe('docs');
      expect((await get('/missing', 'fr.localhost')).status).toBe(404);
    },
  );

  specTest(
    'runs a rewrite merged from another plugin in the root plugin',
    { feature, requirement, check: 'a-merged-plugin-rewrite-runs-in-the-root-plugin' },
    async () => {
      const site = new HttpPlugin().get('/fr', () => new HttpResponse('accueil')).rewrite(frenchHost);
      const prefixed = new HttpPlugin().get('/fr/blog', () => new HttpResponse('blogue')).rewrite(frenchHost);
      const root = new HttpPlugin({ port: 0 }).merge(site).mergeWithPrefix(prefixed, '', (path) => path);
      const get = await serve(root);
      expect(await (await get('/', 'fr.localhost')).text()).toBe('accueil');
      expect(await (await get('/blog', 'fr.localhost')).text()).toBe('blogue');
    },
  );
});
