import { afterAll, beforeEach, describe, expect, it, mock } from 'bun:test';

const realUtils = { ...(await import('@putnami/utils')) };
const mockAppend = mock();
const mockAppendHead = mock();
const mockAddImportModule = mock((file: string) => `mod_${file.replace(/[^\w]/g, '_')}`);
const mockWrite = mock();
const mockFileExists = mock(() => false);

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
});
