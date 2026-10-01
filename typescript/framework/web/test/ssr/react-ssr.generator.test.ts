import { afterAll, describe, expect, it, mock } from 'bun:test';
import { ReactApplicationGenerator } from '../../src/ssr/generator/react-ssr.generator';

// bun's `mock.restore()` does not undo `mock.module()`: keep the real module so
// the mock below can be removed after this file, instead of leaking into the
// suites that run after it.
const realUtils = { ...(await import('@putnami/utils')) };

// Mock GeneratorHelper
const mockAppend = mock();
const mockAppendHead = mock();
const mockAddImportDefault = mock((path: string) => `ShouldImportDefault(${path})`);
const mockAddImportModule = mock((path: string) => `ShouldImportModule(${path})`);
const mockGetModuleName = mock((path: string) => `ShouldGetModuleName(${path})`);
const mockGetLazyImport = mock((path: string) => {
  let moduleSource = path.replace(/\.(ts|tsx|js|jsx)$/, '');
  if (moduleSource.charAt(0) !== '.') {
    moduleSource = `./${moduleSource}`;
  }
  return `() => import('${moduleSource}')`;
});
const mockWrite = mock();
const mockFileExists = mock(() => false);

mock.module('@putnami/utils', () => ({
  GeneratorHelper: class {
    constructor() {
      // biome-ignore lint/correctness/noConstructorReturn: mocking class behavior
      return {
        append: mockAppend,
        appendHead: mockAppendHead.mockReturnThis(),
        addImportDefault: mockAddImportDefault,
        addImportModule: mockAddImportModule,
        getModuleName: mockGetModuleName,
        getLazyImport: mockGetLazyImport,
        write: mockWrite,
      };
    }
  },
  getDirectoryName: (path: string) => path.split('/').slice(0, -1).join('/'),
  getProjectRoot: () => '/root',
  joinPath: (...args: string[]) => args.join('/'),
  relativePath: (_from: string, to: string) => to,
  toPosixPath: (path: string) => path.replaceAll('\\', '/'),
  fileExists: mockFileExists,
}));

describe('ReactApplicationGenerator', () => {
  afterAll(() => {
    mock.module('@putnami/utils', () => realUtils);
  });

  const createGenerator = () => new ReactApplicationGenerator('/path/to/loader');

  // Reset mocks before each test
  const resetMocks = () => {
    mockAppend.mockClear();
    mockAppendHead.mockClear();
    mockAddImportDefault.mockClear();
    mockAddImportModule.mockClear();
    mockGetModuleName.mockClear();
    mockGetLazyImport.mockClear();
    mockWrite.mockClear();
    mockFileExists.mockClear();
  };

  it('initializes correctly', () => {
    resetMocks();
    createGenerator();
    expect(mockAppendHead).toHaveBeenCalled();
    expect(mockAppend).toHaveBeenCalledWith('export default new ReactApplication()');
  });

  describe('addLayout', () => {
    it('imports the layout eagerly so its .secure()/middleware registers at registration time', () => {
      resetMocks();
      const generator = createGenerator();
      resetMocks(); // Clear init calls

      generator.addLayout('dashboard/layout.tsx');

      // Security: the layout must be an eager import (not a lazy `() => import`).
      // A lazy layout only populates the server middleware map on first render,
      // after child pages have already registered without it — a fail-open auth
      // bypass for the generated (lazy-by-default) app. The loader stays lazy.
      expect(mockAddImportModule).toHaveBeenCalledWith('src/app/dashboard/layout.tsx');
      expect(mockGetLazyImport).not.toHaveBeenCalledWith('src/app/dashboard/layout.tsx');
      const expectedCall = `  .reactLayout('/dashboard', { layout: ShouldImportModule(src/app/dashboard/layout.tsx), loader: undefined })`;
      expect(mockAppend).toHaveBeenCalledWith(expectedCall);
    });
  });

  describe('addPage', () => {
    it('generates .reactPage call with lazy imports', () => {
      resetMocks();
      const generator = createGenerator();
      resetMocks();

      generator.addPage('dashboard/page.tsx');

      expect(mockGetLazyImport).toHaveBeenCalledWith('src/app/dashboard/page.tsx');
      const expectedCall = `  .reactPage('/dashboard', { page: () => import('./src/app/dashboard/page'), loader: undefined, action: undefined})`;
      expect(mockAppend).toHaveBeenCalledWith(expectedCall);
    });

    it('applies a route prefix for multi-root scans', () => {
      resetMocks();
      const generator = createGenerator();
      resetMocks();

      generator.addPage('page.tsx', {
        routePrefix: '/projects',
        relativeGenDir: '../../../src/projects/web',
        scannedDir: '/root/src/projects/web',
      });

      expect(mockGetLazyImport).toHaveBeenCalledWith('../../../src/projects/web/page.tsx');
      const expectedCall =
        "  .reactPage('/projects', { page: () => import('../../../src/projects/web/page'), loader: undefined, action: undefined})";
      expect(mockAppend).toHaveBeenCalledWith(expectedCall);
    });

    it('emits the static option when a page declares .static()', () => {
      resetMocks();
      const generator = createGenerator();
      resetMocks();

      generator.addPage('blog/page.tsx', { staticMeta: { mode: 'isr', revalidate: { seconds: 60 } } });

      const expectedCall =
        `  .reactPage('/blog', { page: () => import('./src/app/blog/page'), loader: undefined, action: undefined` +
        `, static: {"mode":"isr","revalidate":{"seconds":60}}})`;
      expect(mockAppend).toHaveBeenCalledWith(expectedCall);
    });
  });

  describe('addNotFound', () => {
    it('generates .reactNotFound call with eager imports (for SEO metadata)', () => {
      resetMocks();
      const generator = createGenerator();
      resetMocks();

      generator.addNotFound('not-found.tsx');

      // Not-found pages use eager imports to ensure SEO metadata renders during SSR
      expect(mockAddImportModule).toHaveBeenCalledWith('src/app/not-found.tsx');
      const expectedCall = `  .reactNotFound('/', { notFound: ShouldImportModule(src/app/not-found.tsx) })`;
      expect(mockAppend).toHaveBeenCalledWith(expectedCall);
    });
  });

  describe('addClientScripts', () => {
    it('generates .reactScript calls with isEntry flag', () => {
      resetMocks();
      const generator = createGenerator();
      resetMocks();

      const scripts = [
        { route: '/react/entry.js', path: 'react/entry.js.gz', isEntry: true },
        { route: '/react/chunk.js', path: 'react/chunk.js.gz', isEntry: false },
      ];

      generator.addClientScripts(scripts);

      expect(mockAppend).toHaveBeenCalledWith("  .reactScript('/react/entry.js', 'react/entry.js.gz')");
      expect(mockAppend).toHaveBeenCalledWith("  .reactScript('/react/chunk.js', 'react/chunk.js.gz', false)");
    });
  });
});
