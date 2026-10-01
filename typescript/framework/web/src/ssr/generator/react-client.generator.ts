import { useConfig } from '@putnami/runtime';
import {
  fileExists,
  GeneratorHelper,
  getDirectoryName,
  getProjectRoot,
  joinPath,
  joinPosixPath,
  relativePath,
  toPosixPath,
} from '@putnami/utils';
import type { ClientRouteNode } from '../../shared/route.types';
import { generatePageId, RouteTreeHelper } from '../../shared/route-tree.utils';
import { PutnamiReactConfig } from '../react-ssr.config';
import { applyRoutePrefix, asRoute } from '../react-ssr.utils';
import { buildEntrypoint } from './build';

interface ReactClientGeneratorFileOptions {
  routePrefix?: string;
  relativeGenDir?: string;
  scannedDir?: string;
}

type ExtendedClientRouteNode = ClientRouteNode & {
  _relativeGenDir?: string;
  _errorRelatifGenDir?: string;
  _notFoundRelatifGenDir?: string;
};

export class ReactClientGenerator {
  root: ClientRouteNode = { path: '/', id: 'root' };
  private readonly routeHelper: RouteTreeHelper<ClientRouteNode>;

  constructor(
    private reactClientPath: string,
    private relativeGenDir = 'src/app',
    private scannedDir = 'src/app',
  ) {
    this.routeHelper = new RouteTreeHelper<ClientRouteNode>(this.root, {
      isLayout: (node) => node.isLayout === true,
      createLayoutNode: (path, id) => ({ path, id, isLayout: true }),
    });
  }

  /**
   * Find or create a route node for layouts only.
   * Delegates to the shared RouteTreeHelper.
   */
  private ensureLayoutRoute(route: string): ClientRouteNode {
    return this.routeHelper.ensureLayoutRoute(route);
  }

  /**
   * Find the nearest parent layout for a given route.
   * Delegates to the shared RouteTreeHelper.
   */
  private findNearestLayout(route: string): { layout: ClientRouteNode; remainingPath: string } {
    return this.routeHelper.findNearestLayout(route);
  }

  addLayout(layoutFile: string, options?: ReactClientGeneratorFileOptions) {
    const route = applyRoutePrefix(asRoute(layoutFile), options?.routePrefix);
    const scannedDir = options?.scannedDir ?? this.scannedDir;
    const relativeGenDir = options?.relativeGenDir ?? this.relativeGenDir;
    const node = this.ensureLayoutRoute(route) as ExtendedClientRouteNode;
    const previousRelatifGenDir = node._relativeGenDir;
    const id = node.id || 'root';
    node.id = `${id}-layout`;
    node.isLayout = true;
    node._relativeGenDir = relativeGenDir;

    if (node.element && !node.element.endsWith('layout.tsx') && !node.element.endsWith('layout')) {
      // There was a page here before the layout, move it to a child
      node.children ||= [];
      let pageRoute = node.children.find((c: ClientRouteNode) => c.path === '' && !c.isLayout) as
        | ExtendedClientRouteNode
        | undefined;
      if (!pageRoute) {
        pageRoute = {
          path: '',
          index: true,
        };
        node.children.push(pageRoute);
      }
      pageRoute.id = `${id}-page`;
      pageRoute.element = node.element;
      pageRoute.isPage = true;
      node.isPage = undefined;
      pageRoute._relativeGenDir = previousRelatifGenDir;
      pageRoute.loader = `pageLoaderHandler()`;
      pageRoute.action = node.action;
    }
    node.element = layoutFile;
    if (fileExists(joinPath(scannedDir, getDirectoryName(layoutFile), 'layout.loader.ts'))) {
      // Thread the framework-owned route pattern so the client builds the
      // `-layout.json` URL from it instead of React Router's experimental
      // `unstable_pattern` field (see client/form/loader.handler.ts).
      node.loader = `layoutLoaderHandler('${route}')`;
      // If an index page already exists under this layout and has no loader yet,
      // reuse the layout loader for the page so the client also sees a loader there.
      if (node.children) {
        const indexChild = node.children.find((c: ClientRouteNode) => c.path === '' && !c.isLayout) as
          | ExtendedClientRouteNode
          | undefined;
        if (indexChild && !indexChild.loader) {
          indexChild.loader = node.loader;
        }
      }
    }
  }

  addPage(pageFile: string, options?: ReactClientGeneratorFileOptions) {
    const route = applyRoutePrefix(asRoute(pageFile), options?.routePrefix);
    const scannedDir = options?.scannedDir ?? this.scannedDir;
    const relativeGenDir = options?.relativeGenDir ?? this.relativeGenDir;
    const { layout, remainingPath } = this.findNearestLayout(route);

    layout.children ||= [];

    // Build the page ID - strip '-layout' suffix to match SSR route IDs
    const pageId = generatePageId(layout.id || 'root', remainingPath);

    // Check if this is an index page (no remaining path)
    const isIndex = remainingPath === '';

    // Find or create the page route
    let pageRoute = layout.children.find((c: ClientRouteNode) => c.path === remainingPath && !c.isLayout) as
      | ExtendedClientRouteNode
      | undefined;

    if (!pageRoute) {
      pageRoute = {
        path: remainingPath,
        id: pageId,
        index: isIndex,
      };
      layout.children.push(pageRoute);
    } else {
      pageRoute.id = pageId;
      if (isIndex) {
        pageRoute.index = true;
      }
    }

    pageRoute.element = pageFile;
    pageRoute.isPage = true;
    pageRoute._relativeGenDir = relativeGenDir;
    if (fileExists(joinPath(scannedDir, getDirectoryName(pageFile), 'loader.ts'))) {
      pageRoute.loader = `pageLoaderHandler()`;
    }
    if (fileExists(joinPath(scannedDir, getDirectoryName(pageFile), 'action.ts'))) {
      pageRoute.action = joinPath(route, 'action.ts');
    }
  }

  addError(errorFile: string, options?: ReactClientGeneratorFileOptions) {
    const route = applyRoutePrefix(asRoute(errorFile), options?.routePrefix);
    const relativeGenDir = options?.relativeGenDir ?? this.relativeGenDir;
    const { layout } = this.findNearestLayout(route) as { layout: ExtendedClientRouteNode; remainingPath: string };
    layout.error = errorFile;
    layout._errorRelatifGenDir = relativeGenDir;
  }

  addNotFound(notFoundFile: string, options?: ReactClientGeneratorFileOptions) {
    const route = applyRoutePrefix(asRoute(notFoundFile), options?.routePrefix);
    const relativeGenDir = options?.relativeGenDir ?? this.relativeGenDir;
    const { layout } = this.findNearestLayout(route) as { layout: ExtendedClientRouteNode; remainingPath: string };
    layout.children ||= [];
    layout.children.push({
      path: '*',
      element: notFoundFile,
      _relativeGenDir: relativeGenDir,
    } as ExtendedClientRouteNode);
    // Store on layout so writeRouteObject can generate a NotFoundErrorBoundary
    layout.notFound = notFoundFile;
    layout._notFoundRelatifGenDir = relativeGenDir;
  }

  write() {
    const generator = new GeneratorHelper(this.reactClientPath);
    generator
      .appendHead(
        `// generated by the ApiPlugin exploring '${toPosixPath(relativePath(getProjectRoot(), this.relativeGenDir))}'`,
      )
      .appendHead(
        `import { createPageElement, ErrorBoundary, NotFoundErrorBoundary, pageLoaderHandler, layoutLoaderHandler, actionHandler, hydratePage, newBrowserRouter, type RouteObject, RouterProvider } from '@putnami/web';`,
      );

    generator.append('const routes: RouteObject[] = [');

    if (this.root) {
      this.writeRouteObject(generator, this.root, '', '', 'ErrorBoundary');
    }

    generator.append('  ];');

    generator.append(`hydratePage('root', routes);`);

    const conf = useConfig(PutnamiReactConfig);
    if (!conf.buildEnable) {
      return;
    }

    generator.write();
  }

  build() {
    return buildEntrypoint(this.reactClientPath, 'hydrate.main.js');
  }

  private writeRouteObject(
    generator: GeneratorHelper,
    route: ClientRouteNode,
    _path: string,
    indent: string,
    _errorElement: string,
    _notFoundComp?: string,
  ) {
    let path = _path;
    let errorElement = _errorElement;
    let notFoundComp = _notFoundComp;
    generator.append(`${indent}{`);
    const subIndent = `${indent}  `;
    const rPath = route.path ?? '';
    let elemName = `${path}_${rPath === '/' ? '' : rPath}`;
    elemName = elemName.replace(/-|\[|\]/g, (match) => (match === '-' ? '_' : ''));
    path = rPath.startsWith('[') ? `:${rPath.slice(1, -1)}` : rPath;

    generator.append(`${subIndent}path: '${path}',`);
    if (route.id) {
      generator.append(`${subIndent}id: '${route.id}',`);
    }
    if (route.index === true) {
      generator.append(`${subIndent}index: true,`);
    }
    if (typeof route.element === 'string') {
      const extRoute = route as ExtendedClientRouteNode;
      const moduleSource = generator.normalizeImport(route.element);
      const importBase = extRoute._relativeGenDir ?? this.relativeGenDir;
      // A native Windows separator in the specifier is an escape sequence.
      const importPath = joinPosixPath(importBase, moduleSource);
      // The server renders a page through createPageElement. The page route
      // must build the same element, or React fails to hydrate the page.
      const rendered = route.isPage ? 'element: createPageElement(d?.component ?? d)' : 'Component: d?.component ?? d';
      generator.append(
        `${subIndent}lazy: () => import('${importPath}').then((m) => { const d = m.default; return { ${rendered}, ...(d?.security ? { handle: { security: d.security } } : {}) }; }),`,
      );
      generator.append(`${subIndent}hydrateFallbackElement: <div style={{ display: 'none' }} />,`);
    }
    if (typeof route.action === 'string') {
      generator.append(`${subIndent}action: actionHandler,`);
    }
    if (typeof route.loader === 'string') {
      generator.append(`${subIndent}loader: ${route.loader},`);
    }
    if (typeof route.error === 'string') {
      const extRoute = route as ExtendedClientRouteNode;
      const errorRelatifGenDir = extRoute._errorRelatifGenDir ?? extRoute._relativeGenDir ?? this.relativeGenDir;
      const errorModName = generator.addImportModule(route.error, errorRelatifGenDir);
      const errorCompName = `${errorModName}_Comp`;
      generator.appendHead(
        // Generated client-side JS: error modules may export { component } or a default component directly
        `const ${errorCompName} = (${errorModName}.default as { component?: unknown })?.component ?? ${errorModName}.default;`,
      );
      errorElement = errorCompName;
    }
    if (typeof route.notFound === 'string') {
      const extRoute = route as ExtendedClientRouteNode;
      const notFoundRelatifGenDir = extRoute._notFoundRelatifGenDir ?? extRoute._relativeGenDir ?? this.relativeGenDir;
      const notFoundModName = generator.addImportModule(route.notFound, notFoundRelatifGenDir);
      const notFoundCompName = `${notFoundModName}_Comp`;
      generator.appendHead(
        // Generated client-side JS: not-found modules may export { component } or a default component directly
        `const ${notFoundCompName} = (${notFoundModName}.default as { component?: unknown })?.component ?? ${notFoundModName}.default;`,
      );
      notFoundComp = notFoundCompName;
    }
    if (notFoundComp) {
      generator.append(
        `${subIndent}errorElement: <NotFoundErrorBoundary notFoundElement={<${notFoundComp} />} fallbackErrorElement={<${errorElement} />} />,`,
      );
    } else {
      generator.append(`${subIndent}errorElement: <${errorElement} />,`);
    }

    if (route.children) {
      generator.append(`${subIndent}children: [`);
      for (const child of Object.values(route.children)) {
        this.writeRouteObject(generator, child, elemName, `${subIndent}  `, errorElement, notFoundComp);
      }
      generator.append(`${subIndent}],`);
    }
    generator.append(`${indent}},`);
  }
}
