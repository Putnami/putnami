import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { HttpRouteInput, HttpRouteMatch, HttpRouteProvenance } from './index';

export type GeneratedHttpRouteProvenance = Omit<HttpRouteProvenance, 'project'>;

/** A framework-owned route fact before the build runner stamps project identity. */
export interface GeneratedHttpRoute extends Omit<HttpRouteInput, 'provenance'> {
  provenance: GeneratedHttpRouteProvenance;
}

/**
 * Convert the native named-segment syntaxes used by the TypeScript routers to
 * the v1 protocol. Named single segments (`[id]`, `:id`, `{id}`) map to `{id}`
 * and a named catch-all (`[...path]` or the router/protocol spelling
 * `{path...}`) maps to the v1 catch-all `{path...}`. Only genuinely
 * unrepresentable segments — anonymous `*` wildcards and regex-like shapes — are
 * left intact so the shared validator emits http_routes.unsupported_pattern.
 */
export function normalizeGeneratedHttpRoutePath(path: string): { match: HttpRouteMatch; path: string } {
  const absolute = path.startsWith('/') ? path : `/${path}`;
  let hasParameter = false;
  const normalized = absolute
    .split('/')
    .map((segment) => {
      // A named catch-all matches one or more segments; it is representable in v1
      // as {name...}. The bracket form is the file-route spelling, the brace form
      // the router/protocol spelling.
      const bracketCatchAll = segment.match(/^\[\.\.\.([A-Za-z_][A-Za-z0-9_]*)\]$/);
      if (bracketCatchAll) {
        hasParameter = true;
        return `{${bracketCatchAll[1]}...}`;
      }
      if (/^\{[A-Za-z_][A-Za-z0-9_]*\.\.\.\}$/.test(segment)) {
        hasParameter = true;
        return segment;
      }
      const bracket = segment.match(/^\[([A-Za-z_][A-Za-z0-9_]*)\]$/);
      if (bracket) {
        hasParameter = true;
        return `{${bracket[1]}}`;
      }
      const colon = segment.match(/^:([A-Za-z_][A-Za-z0-9_]*)$/);
      if (colon) {
        hasParameter = true;
        return `{${colon[1]}}`;
      }
      if (/^\{[A-Za-z_][A-Za-z0-9_]*\}$/.test(segment)) hasParameter = true;
      return segment;
    })
    .join('/');
  return { match: hasParameter ? 'template' : 'exact', path: normalized };
}

/** Stamp project identity and write one framework hook's deterministic fragment. */
export function writeHttpRoutesFragment(
  projectRoot: string,
  fragmentName: string,
  project: string,
  routes: readonly GeneratedHttpRoute[],
): string {
  if (!/^[A-Za-z0-9._-]+$/.test(fragmentName)) throw new Error(`invalid HTTP route fragment name: ${fragmentName}`);
  const directory = join(projectRoot, '.gen', 'http-routes.d');
  const output = join(directory, `${fragmentName}.json`);
  mkdirSync(directory, { recursive: true });
  const fragment = {
    routes: routes.map((route) => ({
      ...route,
      methods: [...route.methods],
      provenance: { project, ...route.provenance },
    })),
  };
  writeFileSync(output, `${JSON.stringify(fragment, null, 2)}\n`);
  return output;
}
