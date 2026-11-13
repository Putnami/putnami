import { type FeatureProof, module } from '@putnami/application';

/**
 * The site's features, each declared on a module of its own. A module composes
 * nothing: it maps the contributions the rest of the composition makes to the
 * authored requirement each one supports, so the build emits their evidence.
 */

const AUDIENCE = 'what-the-site-records-is-what-the-notice-says';

/** The analytics database, its migration and probe, and the events config. */
export function audienceMeasurementFeature() {
  const proves: FeatureProof[] = [
    { requirement: AUDIENCE, contribution: { kind: 'config', key: 'analytics' } },
    { requirement: AUDIENCE, contribution: { kind: 'discoverer', subkind: 'config', key: 'analytics' } },
    { requirement: AUDIENCE, contribution: { kind: 'discoverer', subkind: 'source', key: 'sql-loader' } },
    { requirement: AUDIENCE, contribution: { kind: 'discoverer', subkind: 'source', key: 'sql:putnami-analytics' } },
    { requirement: AUDIENCE, contribution: { kind: 'health', subkind: 'health', key: 'database' } },
    { requirement: AUDIENCE, contribution: { kind: 'infra', subkind: 'database', key: 'analytics' } },
    { requirement: AUDIENCE, contribution: { kind: 'migration', subkind: 'sql', key: 'putnami-analytics' } },
  ];
  return module('audience-measurement').feature({
    id: 'putnami-dev/cookieless-audience-measurement',
    name: 'Cookieless audience measurement',
    outcome:
      'The site knows which pages are read, which searches find nothing, and which platforms the CLI is downloaded for, without a cookie, a consent banner, or a third party — and a visitor can read exactly what is recorded on a page the footer links from',
    owner: 'putnami.dev',
    proves,
  });
}

/** The documentation sources the site reads from other domains. */
const DOCUMENTATION_SNAPSHOTS = [
  'go-framework',
  'method',
  'monorepo-tool',
  'observe',
  'python-surface',
  'stack',
  'typescript-framework',
  'workspace',
];

/** The contracts of other domains the site references. */
const DOCUMENTATION_REFERENCES = ['application-runtime', 'protocol-contracts'];

/**
 * The generated route loaders the evidence pass publishes. The React route
 * loader joins the manifest when the build reconciles its loaders, after the
 * evidence is written, so no proof names it.
 */
const ROUTE_LOADERS = ['api-loader', 'static-loader'];

/** The access contracts, the route inventory and the navigation config. */
export function publicDocumentationFeature() {
  const sources = 'documentation-sources-are-version-addressed-snapshots';
  const routes = 'route-inventory-is-complete';
  const tree = 'agent-surfaces-track-the-docs-tree';
  const proves: FeatureProof[] = [
    ...DOCUMENTATION_SNAPSHOTS.map(
      (name): FeatureProof => ({
        requirement: sources,
        contribution: { kind: 'domainAccess', subkind: 'snapshot', key: `public-docs.${name}-documentation.v1` },
      }),
    ),
    ...DOCUMENTATION_REFERENCES.map(
      (name): FeatureProof => ({
        requirement: sources,
        contribution: { kind: 'domainAccess', subkind: 'reference', key: `public-docs.${name}.v1` },
      }),
    ),
    { requirement: routes, contribution: { kind: 'schema', subkind: 'openapi', key: 'openapi' } },
    ...ROUTE_LOADERS.map(
      (loader): FeatureProof => ({
        requirement: routes,
        contribution: { kind: 'schema', subkind: 'route', key: loader },
      }),
    ),
    { requirement: tree, contribution: { kind: 'config', key: 'app' } },
    { requirement: tree, contribution: { kind: 'discoverer', subkind: 'config', key: 'app' } },
  ];
  return module('public-documentation').feature({
    id: 'putnami-dev/public-documentation',
    name: 'Public documentation site',
    outcome:
      'A visitor reaches what Putnami is, how to install it, which language surfaces exist, and where each package and feature is documented, from one site, without reading the repository',
    owner: 'putnami.dev',
    proves,
  });
}
