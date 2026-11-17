import {
  type GeneratedHttpRoute,
  type HttpRouteProof,
  httpRouteAdmitsPath,
  normalizeGeneratedHttpRoutePath,
} from '@putnami/application/http-routes';

export interface WebPageRouteFact {
  route: string;
  evidencePath: string;
  hasAction: boolean;
  /** Concrete paths registered by a finite static catch-all expansion. */
  expandedPaths?: string[];
  /** The feature requirements the page module's `proves` export declares. */
  proves?: readonly HttpRouteProof[];
}

/**
 * Convert file-system pages/actions into v1 route facts. A page's `proves`
 * declarations go to each of its page routes whose path admits their `at`;
 * a declaration that no route of its page admits throws, naming the page.
 */
export function buildWebPageHttpRoutes(pages: readonly WebPageRouteFact[]): GeneratedHttpRoute[] {
  const routes: GeneratedHttpRoute[] = [];
  // Per page file: every declaration, and the ones a route of the page admits.
  // One page file yields several facts under i18n, one per locale.
  const declared = new Map<string, Map<string, HttpRouteProof>>();
  const admitted = new Map<string, Set<string>>();
  for (const page of pages) {
    const evidencePath = page.evidencePath.replaceAll('\\', '/');
    const pagePaths =
      page.route.includes('*') && page.expandedPaths?.length ? [...new Set(page.expandedPaths)].sort() : [page.route];
    const pageDeclared = declared.get(evidencePath) ?? new Map<string, HttpRouteProof>();
    const pageAdmitted = admitted.get(evidencePath) ?? new Set<string>();
    declared.set(evidencePath, pageDeclared);
    admitted.set(evidencePath, pageAdmitted);
    for (const proof of page.proves ?? []) pageDeclared.set(proofKey(proof), proof);
    for (const path of pagePaths) {
      const normalized = normalizeGeneratedHttpRoutePath(path);
      const proves = (page.proves ?? []).filter((proof) => httpRouteAdmitsPath(normalized, proof.at));
      for (const proof of proves) pageAdmitted.add(proofKey(proof));
      routes.push({
        ...normalized,
        methods: ['GET', 'HEAD'],
        publicEdge: true,
        provenance: {
          package: '@putnami/web',
          sourceKind: 'file-route',
          evidencePath,
        },
        ...(proves.length > 0 ? { proves: proves.map((proof) => ({ ...proof })) } : {}),
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
  const errors: string[] = [];
  for (const [evidencePath, pageDeclared] of declared) {
    for (const [key, proof] of pageDeclared) {
      if (!admitted.get(evidencePath)?.has(key))
        errors.push(
          `${evidencePath}: proves at '${proof.at}' (${proof.feature}#${proof.requirement}) is not a path this page serves`,
        );
    }
  }
  if (errors.length > 0) throw new Error(`${errors.length} page proves error(s):\n${errors.join('\n')}`);
  return routes;
}

function proofKey(proof: HttpRouteProof): string {
  return `${proof.feature}\0${proof.requirement}\0${proof.at}`;
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

/** The sitemap a localized application serves from its public output. */
export function buildWebSitemapHttpRoutes(publicFolder: string): GeneratedHttpRoute[] {
  return [
    {
      match: 'exact',
      path: '/sitemap.xml',
      methods: ['GET', 'HEAD'],
      publicEdge: true,
      provenance: {
        package: '@putnami/web',
        sourceKind: 'public-file',
        evidencePath: `.gen/${publicFolder.replace(/^\/+|\/+$/g, '')}/sitemap.xml`,
      },
    },
  ];
}

/** The action module beside the page at `pageEvidencePath`, a slash-separated path. */
function actionEvidencePath(pageEvidencePath: string): string {
  const slash = pageEvidencePath.lastIndexOf('/');
  return slash < 0 ? 'action.ts' : `${pageEvidencePath.slice(0, slash)}/action.ts`;
}
