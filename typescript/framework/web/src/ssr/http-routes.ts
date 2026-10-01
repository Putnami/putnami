import { type GeneratedHttpRoute, normalizeGeneratedHttpRoutePath } from '@putnami/application/http-routes';

export interface WebPageRouteFact {
  route: string;
  evidencePath: string;
  hasAction: boolean;
  /** Concrete paths registered by a finite static catch-all expansion. */
  expandedPaths?: string[];
}

/** Convert file-system pages/actions into v1 route facts. */
export function buildWebPageHttpRoutes(pages: readonly WebPageRouteFact[]): GeneratedHttpRoute[] {
  const routes: GeneratedHttpRoute[] = [];
  for (const page of pages) {
    const evidencePath = page.evidencePath.replaceAll('\\', '/');
    const pagePaths =
      page.route.includes('*') && page.expandedPaths?.length ? [...new Set(page.expandedPaths)].sort() : [page.route];
    for (const path of pagePaths) {
      const normalized = normalizeGeneratedHttpRoutePath(path);
      routes.push({
        ...normalized,
        methods: ['GET', 'HEAD'],
        publicEdge: true,
        provenance: {
          package: '@putnami/web',
          sourceKind: 'file-route',
          evidencePath,
        },
      });
      if (page.hasAction) {
        routes.push({
          ...normalized,
          methods: ['POST'],
          publicEdge: true,
          provenance: {
            package: '@putnami/web',
            sourceKind: 'file-route',
            evidencePath: actionEvidencePath(evidencePath),
          },
        });
      }
    }
  }
  return routes;
}

/** Collapse hashed client bundles to the bounded directories the build owns. */
export function buildWebAssetHttpRoutes(publicFolder: string, assetRoutes: readonly string[]): GeneratedHttpRoute[] {
  const prefixes = new Set<string>();
  for (const route of assetRoutes) {
    const absolute = route.startsWith('/') ? route : `/${route}`;
    const slash = absolute.lastIndexOf('/');
    if (slash > 0) prefixes.add(absolute.slice(0, slash + 1));
  }
  return [...prefixes].sort().map((path) => ({
    match: 'prefix',
    path,
    methods: ['GET', 'HEAD'],
    publicEdge: true,
    provenance: {
      package: '@putnami/web',
      sourceKind: 'static-mount',
      evidencePath: `.gen/${publicFolder.replace(/^\/+|\/+$/g, '')}${path}`.replace(/\/$/, ''),
    },
  }));
}

/** The action module beside the page at `pageEvidencePath`, a slash-separated path. */
function actionEvidencePath(pageEvidencePath: string): string {
  const slash = pageEvidencePath.lastIndexOf('/');
  return slash < 0 ? 'action.ts' : `${pageEvidencePath.slice(0, slash)}/action.ts`;
}
