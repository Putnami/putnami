import { afterAll, beforeEach, describe, expect, it, mock } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { createMemoryRouter, type RouteObject } from 'react-router';

const realUtils = { ...(await import('@putnami/utils')) };
const mockAppend = mock();
const mockAppendHead = mock();
const mockAddImportModule = mock((file: string) => `mod_${file.replace(/[^\w]/g, '_')}`);
const mockWrite = mock();
const mockFileExists = mock((_path: string) => false);

mock.module('@putnami/utils', () => ({
  fileExists: mockFileExists,
  GeneratorHelper: class {
    constructor(public fileName: string) {}

    normalizeImport(file: string) {
      let moduleSource = file.replace(/\.(ts|tsx|js|jsx)$/, '');
      if (moduleSource.charAt(0) === '/') {
        moduleSource = `.${moduleSource}`;
      } else if (moduleSource.charAt(0) !== '.') {
        moduleSource = `./${moduleSource}`;
      }
      return moduleSource;
    }

    appendHead(line: string) {
      mockAppendHead(line);
      return this;
    }

    append(line: string) {
      mockAppend(line);
      return this;
    }

    addImportModule(file: string, _relativeTo = '') {
      return mockAddImportModule(file);
    }

    write() {
      mockWrite(this.fileName);
      return this.fileName;
    }
  },
  getDirectoryName: (path: string) => path.split('/').slice(0, -1).join('/'),
  getProjectRoot: () => '/project',
  joinPath: (...parts: string[]) =>
    parts
      .filter(Boolean)
      .join('/')
      .replace(/\/\.\//g, '/')
      .replace(/\/+/g, '/'),
  joinPosixPath: realUtils.joinPosixPath,
  relativePath: (_from: string, to: string) => to,
  toPosixPath: realUtils.toPosixPath,
}));

const { ReactClientGenerator } = await import('../../src/ssr/generator/react-client.generator');

type Generator = InstanceType<typeof ReactClientGenerator>;

/** Addresses the generated `actionHandler` was posted to. */
const posted: string[] = [];

/**
 * Evaluate the routes a generator writes, with the route components left out
 * and `actionHandler` recording the address it was posted to.
 */
function generatedRoutes(generator: Generator): RouteObject[] {
  mockAppend.mockReset();
  generator.write();
  const source = mockAppend.mock.calls
    .map(([line]) => String(line))
    .filter((line) => !/^\s*(lazy|hydrateFallbackElement|errorElement):/.test(line))
    .filter((line) => !line.startsWith('hydratePage('))
    .join('\n');
  const code = new Bun.Transpiler({ loader: 'ts' }).transformSync(`${source}\nreturn routes;`);
  const actionHandler = async ({ request }: { request: Request }) => {
    posted.push(new URL(request.url).pathname);
    return { ok: true };
  };
  return new Function('actionHandler', 'pageLoaderHandler', 'layoutLoaderHandler', code)(
    actionHandler,
    () => () => null,
    () => () => null,
  ) as RouteObject[];
}

/**
 * Submit a fetcher post through the routes, as `useFetcher().submit` does,
 * and answer the address the action saw, or the status of the error.
 */
async function submit(routes: RouteObject[], href: string): Promise<{ posted?: string; status?: number }> {
  posted.length = 0;
  const router = createMemoryRouter(routes, { initialEntries: [href.split('?')[0] ?? '/'] });
  await router.initialize();
  const settled = new Promise<void>((resolve) => {
    const unsubscribe = router.subscribe((state) => {
      if (state.fetchers.get('submit')?.state === 'idle' || Object.keys(state.errors ?? {}).length > 0) {
        unsubscribe();
        resolve();
      }
    });
  });
  const leaf = router.state.matches.at(-1)?.route.id ?? 'root';
  await router.fetch('submit', leaf, href, { formMethod: 'post', formData: new FormData() });
  await settled;
  const error = Object.values(router.state.errors ?? {})[0] as { status?: number } | undefined;
  router.dispose();
  return { posted: posted[0], status: error?.status };
}

describe('ReactClientGenerator', () => {
  afterAll(() => {
    mock.module('@putnami/utils', () => realUtils);
  });

  beforeEach(() => {
    mockAppend.mockReset();
    mockAppendHead.mockReset();
    mockAddImportModule.mockReset();
    mockAddImportModule.mockImplementation((file: string) => `mod_${file.replace(/[^\w]/g, '_')}`);
    mockWrite.mockReset();
    mockFileExists.mockReset();
    mockFileExists.mockReturnValue(false);
  });

  it('initializes with a root route', () => {
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');
    expect(generator.root).toEqual({ path: '/', id: 'root' });
  });

  it('adds layouts and nested layouts to the route tree', () => {
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');

    generator.addLayout('dashboard/layout.tsx');
    generator.addLayout('dashboard/settings/layout.tsx');

    expect(generator.root.children?.[0]?.path).toBe('dashboard');
    expect(generator.root.children?.[0]?.children?.[0]?.path).toBe('settings');
  });

  it('nests a page inside a layout whose parent path has no layout', () => {
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');

    generator.addLayout('w/[workspace]/layout.tsx');
    generator.addPage('w/[workspace]/settings/page.tsx');

    const w = generator.root.children?.[0];
    const workspace = w?.children?.[0];
    expect(generator.root.children).toHaveLength(1);
    expect({ path: w?.path, id: w?.id }).toEqual({ path: 'w', id: 'root-w' });
    expect({ path: workspace?.path, id: workspace?.id }).toEqual({ path: ':workspace', id: 'root-w-workspace-layout' });
    expect(workspace?.children?.map((c) => ({ path: c.path, id: c.id }))).toEqual([
      { path: 'settings', id: 'root-w-workspace-settings-page' },
    ]);
  });

  it('moves an existing page under a layout and wires the layout loader', () => {
    mockFileExists.mockImplementation((path: string) => path.endsWith('/dashboard/layout.loader.ts'));
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');

    generator.addPage('dashboard/page.tsx');
    generator.addLayout('dashboard/layout.tsx');

    const dashboardNode = generator.root.children?.find((child) => child.path === 'dashboard');
    const indexChild = dashboardNode?.children?.find((child) => child.path === '');

    // The layout loader is threaded with the framework-owned route pattern so the
    // client does not rely on React Router's experimental `unstable_pattern`.
    expect(dashboardNode?.loader).toBe("layoutLoaderHandler('/dashboard')");
    expect(indexChild?.loader).toBe('pageLoaderHandler()');
  });

  it('adds pages with colocated loader and action handlers', () => {
    mockFileExists.mockImplementation(
      (path: string) => path.endsWith('/tasks/loader.ts') || path.endsWith('/tasks/action.ts'),
    );
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');

    generator.addPage('tasks/page.tsx');

    const pageNode = generator.root.children?.find((child) => child.path === 'tasks');
    expect(pageNode?.loader).toBe('pageLoaderHandler()');
    expect(pageNode?.action).toBe('/tasks/action.ts');
  });

  it('supports route prefixes, errors, not-found routes, and route groups', () => {
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');

    generator.addPage('page.tsx', { routePrefix: '/projects' });
    generator.addLayout('admin/layout.tsx');
    generator.addError('admin/error.tsx');
    generator.addNotFound('admin/not-found.tsx');
    generator.addPage('(auth)/login/page.tsx');

    expect(generator.root.children?.find((child) => child.path === 'projects')).toBeDefined();
    expect(generator.root.children?.find((child) => child.path === 'login')).toBeDefined();
    const adminNode = generator.root.children?.find((child) => child.path === 'admin');
    expect(adminNode?.error).toBe('admin/error.tsx');
    expect(adminNode?.children?.some((child) => child.path === '*')).toBe(true);
  });

  it('writes imports, lazy routes, loaders, actions, and not-found error boundaries', () => {
    mockFileExists.mockImplementation(
      (path: string) =>
        path.endsWith('/dashboard/layout.loader.ts') ||
        path.endsWith('/dashboard/loader.ts') ||
        path.endsWith('/dashboard/action.ts'),
    );
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');

    generator.addLayout('dashboard/layout.tsx');
    generator.addPage('dashboard/page.tsx');
    generator.addError('dashboard/error.tsx');
    generator.addNotFound('dashboard/not-found.tsx');
    generator.write();

    expect(
      mockAppend.mock.calls.some(([line]) => String(line).includes("lazy: () => import('src/app/dashboard/layout')")),
    ).toBe(true);
    expect(
      mockAppend.mock.calls.some(([line]) => String(line).includes("loader: layoutLoaderHandler('/dashboard')")),
    ).toBe(true);
    expect(mockAppend.mock.calls.some(([line]) => String(line).includes('loader: pageLoaderHandler()'))).toBe(true);
    expect(mockAppend.mock.calls.some(([line]) => String(line).includes('action: actionHandler'))).toBe(true);
    expect(mockAppend.mock.calls.some(([line]) => String(line).includes('NotFoundErrorBoundary'))).toBe(true);
    expect(mockWrite).toHaveBeenCalledWith('/client.tsx');
  });

  it('renders a page through the page boundary, and a layout or a not-found page as a plain component', () => {
    const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');

    // The dashboard page is added before its layout, so the layout moves it.
    generator.addPage('dashboard/page.tsx');
    generator.addLayout('dashboard/layout.tsx');
    generator.addPage('dashboard/settings/page.tsx');
    generator.addNotFound('dashboard/not-found.tsx');
    generator.write();

    const lazyLine = (file: string) =>
      mockAppend.mock.calls.map(([line]) => String(line)).find((line) => line.includes(`import('src/app/${file}')`));
    const pageElement = 'element: createPageElement(d?.component ?? d)';
    const plainComponent = 'Component: d?.component ?? d';

    expect(lazyLine('dashboard/page')).toContain(pageElement);
    expect(lazyLine('dashboard/settings/page')).toContain(pageElement);
    expect(lazyLine('dashboard/layout')).toContain(plainComponent);
    expect(lazyLine('dashboard/not-found')).toContain(plainComponent);
    expect(mockAppendHead.mock.calls.some(([line]) => String(line).includes('createPageElement,'))).toBe(true);
  });

  it('writes forward-slash lazy imports and routes from Windows-separated paths', () => {
    // On Windows the scan and the generated directory carry native separators.
    // A backslash in a specifier is an escape sequence (`\n` in `..\not-found`).
    const generator = new ReactClientGenerator('/client.tsx', '..\\..\\..\\src\\app', '/scanned');

    generator.addPage('page.tsx');
    generator.addPage('blog\\[slug]\\page.tsx');
    generator.addNotFound('not-found.tsx');
    generator.write();

    const lines = mockAppend.mock.calls.map(([line]) => String(line));
    expect(lines.some((line) => line.includes("lazy: () => import('../../../src/app/page')"))).toBe(true);
    expect(lines.some((line) => line.includes("lazy: () => import('../../../src/app/blog/[slug]/page')"))).toBe(true);
    expect(lines.some((line) => line.includes("lazy: () => import('../../../src/app/not-found')"))).toBe(true);
    expect(lines.some((line) => line.includes("path: 'blog/:slug'"))).toBe(true);
    expect(lines.filter((line) => line.includes('\\'))).toEqual([]);
  });

  describe('a layout that shares its address with a page', () => {
    // Each directory holds layout.tsx, page.tsx and action.ts, and `href` is
    // the address a form in the page posts to. Generation adds a scan root's
    // layouts before its pages, so a page comes first only from another scan
    // root. At a multi-segment address that order does not nest the page under
    // the layout at all, so the page-first order is covered where it does.
    const directories = [
      { name: 'a plain directory', dir: 'n/', href: '/n', pageFirst: true },
      { name: 'the root directory', dir: '', href: '/', pageFirst: true },
      { name: 'a parameter directory', dir: 'r/[run]/', href: '/r/42', pageFirst: false },
    ];
    const orders = [
      {
        name: 'the layout first',
        add: (g: Generator, dir: string) => (g.addLayout(`${dir}layout.tsx`), g.addPage(`${dir}page.tsx`)),
      },
      {
        name: 'the page first',
        add: (g: Generator, dir: string) => (g.addPage(`${dir}page.tsx`), g.addLayout(`${dir}layout.tsx`)),
      },
    ];

    for (const { name, dir, href, pageFirst } of directories) {
      for (const order of pageFirst ? orders : orders.slice(0, 1)) {
        specTest(
          `routes a post to ${name} to its page action, with ${order.name}`,
          {
            feature: 'typescript/web-application-delivery',
            requirement: 'route-graph',
            check: 'a-post-to-a-layout-address-reaches-its-index-page-action',
          },
          async () => {
            mockFileExists.mockImplementation((path: string) => path.endsWith(`/scanned/${dir}action.ts`));
            const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');
            order.add(generator, dir);

            const routes = generatedRoutes(generator);
            expect(await submit(routes, href)).toEqual({ posted: href, status: undefined });
            expect(await submit(routes, `${href}?index`)).toEqual({ posted: href, status: undefined });
          },
        );
      }

      specTest(
        `answers 405 to a post to ${name} whose page has no action`,
        {
          feature: 'typescript/web-application-delivery',
          requirement: 'route-graph',
          check: 'a-layout-without-an-index-page-action-answers-405',
        },
        async () => {
          for (const order of pageFirst ? orders : orders.slice(0, 1)) {
            const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');
            order.add(generator, dir);

            const routes = generatedRoutes(generator);
            expect(await submit(routes, href)).toEqual({ posted: undefined, status: 405 });
            expect(await submit(routes, `${href}?index`)).toEqual({ posted: undefined, status: 405 });
          }
        },
      );
    }

    it('gives the layout its index page action in both registration orders, and no other page action', () => {
      mockFileExists.mockImplementation((path: string) => path.endsWith('/action.ts'));
      for (const order of orders) {
        const generator = new ReactClientGenerator('/client.tsx', './src/app', '/scanned');
        order.add(generator, 'n/');
        generator.addPage('n/settings/page.tsx');

        const layout = generator.root.children?.find((child) => child.path === 'n');
        const index = layout?.children?.find((child) => child.index === true);
        const settings = layout?.children?.find((child) => child.path === 'settings');
        expect(layout?.action).toBe('/n/action.ts');
        expect(index?.action).toBe('/n/action.ts');
        expect(settings?.action).toBe('/n/settings/action.ts');
      }
    });
  });
});
