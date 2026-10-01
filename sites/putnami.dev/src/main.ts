import './activate';
import { analytics } from '@putnami/analytics';
import {
  application,
  api,
  config,
  DarcPlugin,
  http,
  logger,
  openapi,
  platform,
  staticFiles,
  trace,
} from '@putnami/application';
import { sql } from '@putnami/database';
import { react } from '@putnami/web';
import { siteEvents } from './analytics';
import './config';
import { publicDocumentationAccess } from './lib/darc/public-docs';
import { contentBundles } from './plugins/content-bundle.plugin';
import { contentOverlay } from './plugins/content-overlay.plugin';
import { prerenderDocs } from './plugins/docs-prerender.plugin';
import { installerRun } from './plugins/installer-run.plugin';
import { publicSurface } from './plugins/public-surface.plugin';
import { schemas } from './plugins/schemas.plugin';
import { searchIndex } from './plugins/search-index.plugin';
import { supportCatalog } from './plugins/support-catalog.plugin';

export const app = () => {
  // The `public-docs` domain's declared cross-domain access, as components. The
  // same instance governs the docs aggregation (docs-prerender consults its
  // snapshots) and reports the contracts it enforced to the capability
  // manifest, so the evidence describes the build that actually happened.
  const documentation = publicDocumentationAccess();

  return (
    application()
      .use(logger())
      .use(config())
      .use(
        http({
          compression: true,
          // Secure-by-default headers are applied automatically; override the CSP
          // to permit the jsDelivr CDN used by the docs site.
          securityHeaders: {
            contentSecurityPolicy: [
              "default-src 'self'",
              "script-src 'self' https://cdn.jsdelivr.net",
              "style-src 'self' 'unsafe-inline'",
              "img-src 'self' data:",
              "font-src 'self'",
              "connect-src 'self'",
              "frame-ancestors 'none'",
              "base-uri 'self'",
              "form-action 'self'",
            ].join('; '),
          },
        }),
      )
      .use(platform({ prefix: '/_' }))
      .use(trace())
      .use(api())
      // Audience measurement, in the site's own database. `sql()` first:
      // the four analytics tables are the plugin's own migration source, and
      // `autoApply` stays off so a deploy applies them in its migrate job and a
      // local run without Postgres still serves. The site is cookieless — a
      // daily-rotating server-side hash, no banner, no third party, no egress.
      //
      // `datasource` is not decoration: it names the datasource the /healthz
      // probe pings. Left out, the probe pings `default`, which this workload
      // declares nowhere — so a production instance with its `analytics`
      // database correctly provisioned would still report the database probe
      // unhealthy, for a pool it never opens.
      .use(sql({ datasource: 'analytics' }))
      .use(react())
      .use(analytics({ events: siteEvents }))
      .use(openapi({ title: 'Putnami', version: '1.0.0', exposeRoute: false }))
      .use(searchIndex({ docsDir: '.gen/public/docs' }))
      .use(contentBundles())
      // Publishes /docs/support from the reviewed putnami.support.json. The docs
      // static-path callback already wrote the same bytes earlier in the build;
      // this registers the file as a shipped asset.
      .use(supportCatalog())
      .use(prerenderDocs(documentation))
      // Evidence, never authority (ADR 0001): one machine row per contract the
      // components above enforced. `putnami architecture validate` joins those
      // rows to the declarations in putnami.architecture.json; registering the
      // plugin cannot create a permission, only a reviewed manifest edit can.
      .use(new DarcPlugin('public-docs-contracts', ...documentation.components()))
      .use(
        schemas({
          // hosted-schemas/ holds byte-identical copies of schemas a published
          // Putnami product owns, such as the `putnami agent-readiness` payload
          // and report. See README.md "Where the published content comes from".
          sources: ['protocols/*/schemas/*.json', 'sites/putnami.dev/hosted-schemas/*/*.json'],
          // infra-aggregated.json is an ephemeral, build-emitted manifest that
          // intentionally shares the putnami-infra.json $id with the per-project
          // schema (PerProjectSchemaURL / aggregatedSchemaURL). Only the
          // developer-facing per-project schema is published at that URL.
          exclude: ['protocols/infra/schemas/infra-aggregated.json'],
        }),
      )
      // Runtime self-update overlay: registered before staticFiles so its
      // dynamic /search/index.json route is in place when staticFiles warms up the
      // baked one. Behavior comes from the `contentOverlay` config section; the
      // bundle identity comes from content.lock.json.
      .use(contentOverlay())
      // /install.sh?run=<command> and /install.ps1?run=<command>: the same
      // script with the command set in its one placeholder line, with the static
      // route's headers. Without ?run= the request falls through to the static
      // route below.
      .use(installerRun())
      .use(staticFiles({ publicFolder: 'public', cacheMaxAge: 2_592_000 }))
      // Canonical public-route inventory: declares the route facts no
      // framework emitter can see (generate.assets copies, the SSG mirror, and
      // the warmup-registered overlay/probe endpoints) so
      // .gen/schema/http-routes.json is a complete putnami.http-routes.v1
      // artifact. Contributes no runtime behavior.
      .use(publicSurface())
  );
};
