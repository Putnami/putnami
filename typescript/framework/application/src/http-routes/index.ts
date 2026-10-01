import { createHash } from 'node:crypto';
import { Buffer } from 'node:buffer';
import { posix as pathPosix } from 'node:path';

export * from './generation';

export const HTTP_ROUTES_PROTOCOL = 'putnami.http-routes.v1' as const;
export const HTTP_ROUTES_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-http-routes-v1.json' as const;

export type HttpRouteMatch = 'exact' | 'prefix' | 'template';
export type HttpRouteSourceKind = 'typed-api' | 'file-route' | 'static-mount' | 'public-file' | 'manual';
export type HttpRouteMethod = 'DELETE' | 'GET' | 'HEAD' | 'OPTIONS' | 'PATCH' | 'POST' | 'PUT';

export interface HttpRouteProvenance {
  project: string;
  package?: string;
  sourceKind: HttpRouteSourceKind;
  evidencePath?: string;
}

export interface HttpRoute {
  match: HttpRouteMatch;
  path: string;
  methods: HttpRouteMethod[];
  publicEdge: boolean;
  provenance: HttpRouteProvenance;
}

/** Input accepted by the canonicalizer before method case/order normalization. */
export interface HttpRouteInput extends Omit<HttpRoute, 'methods'> {
  methods: readonly string[];
}

export interface HttpRoutesManifest {
  $schema: typeof HTTP_ROUTES_SCHEMA_URL;
  protocol: typeof HTTP_ROUTES_PROTOCOL;
  routes: HttpRoute[];
  digest: string;
}

export interface HttpRoutesDiagnostic {
  severity: 'error';
  code: string;
  message: string;
  field?: string;
}

export interface ParseHttpRoutesResult {
  manifest?: HttpRoutesManifest;
  diagnostics: HttpRoutesDiagnostic[];
}

export const HTTP_ROUTES_ERROR_CODES = {
  parseError: 'http_routes.parse_error',
  unknownField: 'http_routes.unknown_field',
  invalidProtocol: 'http_routes.invalid_protocol',
  invalidSchema: 'http_routes.invalid_schema',
  invalidDigest: 'http_routes.invalid_digest',
  digestMismatch: 'http_routes.digest_mismatch',
  invalidMatch: 'http_routes.invalid_match',
  invalidPath: 'http_routes.invalid_path',
  unsupportedPattern: 'http_routes.unsupported_pattern',
  invalidMethod: 'http_routes.invalid_method',
  duplicateMethod: 'http_routes.duplicate_method',
  missingProvenance: 'http_routes.missing_provenance',
  invalidSourceKind: 'http_routes.invalid_source_kind',
  invalidEvidencePath: 'http_routes.invalid_evidence_path',
  duplicateRoute: 'http_routes.duplicate_route',
  visibilityOverlap: 'http_routes.visibility_overlap',
  serializationError: 'http_routes.serialization_error',
} as const;

const VALID_METHODS = new Set<HttpRouteMethod>(['DELETE', 'GET', 'HEAD', 'OPTIONS', 'PATCH', 'POST', 'PUT']);
const VALID_MATCHES = new Set<HttpRouteMatch>(['exact', 'prefix', 'template']);
const VALID_SOURCE_KINDS = new Set<HttpRouteSourceKind>([
  'typed-api',
  'file-route',
  'static-mount',
  'public-file',
  'manual',
]);
const PARAMETER_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;
const DIGEST = /^sha256:[0-9a-f]{64}$/;

/**
 * Strictly parse and validate a v1 inventory. Unknown fields, unsafe route
 * patterns, visibility overlaps, and digest drift all fail closed.
 */
export function parseAndValidateHttpRoutes(text: string): ParseHttpRoutesResult {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (cause) {
    return { diagnostics: [error(HTTP_ROUTES_ERROR_CODES.parseError, '', String(cause))] };
  }

  const shapeDiagnostics: HttpRoutesDiagnostic[] = [];
  if (!isRecord(value)) {
    return { diagnostics: [error(HTTP_ROUTES_ERROR_CODES.parseError, '', 'manifest must be a JSON object')] };
  }
  validateObjectShape(
    value,
    ['$schema', 'protocol', 'routes', 'digest'],
    ['$schema', 'protocol', 'routes', 'digest'],
    '',
    shapeDiagnostics,
  );
  if (typeof value['$schema'] !== 'string')
    shapeDiagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, '$schema', '$schema must be a string'));
  if (typeof value['protocol'] !== 'string')
    shapeDiagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, 'protocol', 'protocol must be a string'));
  if (!Array.isArray(value['routes']))
    shapeDiagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, 'routes', 'routes must be an array'));
  if (typeof value['digest'] !== 'string')
    shapeDiagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, 'digest', 'digest must be a string'));

  const routesValue = value['routes'];
  if (Array.isArray(routesValue)) {
    routesValue.forEach((route, index) => {
      validateRouteShape(route, `routes[${index}]`, shapeDiagnostics);
    });
  }
  if (shapeDiagnostics.length > 0) return { diagnostics: shapeDiagnostics };

  const manifest = value as unknown as HttpRoutesManifest;
  const diagnostics = validateHttpRoutesManifest(manifest);
  if (diagnostics.length > 0) return { diagnostics };
  return { manifest, diagnostics: [] };
}

/** Validate an already-decoded manifest and verify its canonical digest. */
export function validateHttpRoutesManifest(manifest: HttpRoutesManifest): HttpRoutesDiagnostic[] {
  const diagnostics: HttpRoutesDiagnostic[] = [];
  if (manifest.$schema !== HTTP_ROUTES_SCHEMA_URL) {
    diagnostics.push(
      error(
        HTTP_ROUTES_ERROR_CODES.invalidSchema,
        '$schema',
        `$schema ${JSON.stringify(manifest.$schema)} is not the canonical v1 schema`,
      ),
    );
  }
  if (manifest.protocol !== HTTP_ROUTES_PROTOCOL) {
    diagnostics.push(
      error(
        HTTP_ROUTES_ERROR_CODES.invalidProtocol,
        'protocol',
        `protocol ${JSON.stringify(manifest.protocol)} is not supported`,
      ),
    );
  }
  diagnostics.push(...validateHttpRoutes(manifest.routes));
  if (!DIGEST.test(manifest.digest)) {
    diagnostics.push(
      error(
        HTTP_ROUTES_ERROR_CODES.invalidDigest,
        'digest',
        'digest must be sha256 followed by 64 lowercase hexadecimal characters',
      ),
    );
  } else if (diagnostics.length === 0) {
    const canonical = canonicalizeHttpRoutes(manifest.routes);
    if (canonical.diagnostics.length > 0) diagnostics.push(...canonical.diagnostics);
    else if (canonical.manifest?.digest !== manifest.digest) {
      diagnostics.push(
        error(
          HTTP_ROUTES_ERROR_CODES.digestMismatch,
          'digest',
          `digest ${JSON.stringify(manifest.digest)} does not match canonical inventory digest ${JSON.stringify(canonical.manifest?.digest)}`,
        ),
      );
    }
  }
  return diagnostics;
}

/**
 * Canonicalize raw route facts without mutating them. Method names are
 * uppercased and sorted before strict validation.
 */
export function canonicalizeHttpRoutes(routes: readonly HttpRouteInput[]): ParseHttpRoutesResult {
  const normalized = routes.map(
    (route): HttpRoute => ({
      match: route.match,
      path: route.path,
      methods: route.methods.map((method) => method.toUpperCase()).sort(compareStrings) as HttpRouteMethod[],
      publicEdge: route.publicEdge,
      provenance: canonicalProvenance(route.provenance),
    }),
  );
  normalized.sort(compareRoutes);
  const diagnostics = validateHttpRoutes(normalized);
  if (diagnostics.length > 0) return { diagnostics };
  const digest = computeDigest(normalized);
  return {
    manifest: {
      $schema: HTTP_ROUTES_SCHEMA_URL,
      protocol: HTTP_ROUTES_PROTOCOL,
      routes: normalized,
      digest,
    },
    diagnostics: [],
  };
}

/** Render the exact cross-language byte form, including one trailing newline. */
export function serializeCanonicalHttpRoutes(
  manifest: Pick<HttpRoutesManifest, 'routes'>,
): ParseHttpRoutesResult & { text?: string } {
  const result = canonicalizeHttpRoutes(manifest.routes);
  if (!result.manifest) return result;
  return { ...result, text: `${escapeLikeGo(JSON.stringify(result.manifest, null, 2))}\n` };
}

/** Validate route-level and cross-route invariants in stable input order. */
export function validateHttpRoutes(routes: readonly HttpRoute[]): HttpRoutesDiagnostic[] {
  const diagnostics: HttpRoutesDiagnostic[] = [];
  routes.forEach((route, index) => {
    diagnostics.push(...validateRoute(route, `routes[${index}]`));
  });
  if (diagnostics.length > 0) return diagnostics;

  for (let left = 0; left < routes.length; left++) {
    for (let right = left + 1; right < routes.length; right++) {
      const a = routes[left];
      const b = routes[right];
      if (!methodsOverlap(a.methods, b.methods)) continue;
      if (sameMatchLanguage(a, b)) {
        diagnostics.push(
          error(
            HTTP_ROUTES_ERROR_CODES.duplicateRoute,
            `routes[${right}]`,
            `route duplicates routes[${left}] for at least one HTTP method`,
          ),
        );
      } else if (a.publicEdge !== b.publicEdge && pathsOverlap(a, b)) {
        diagnostics.push(
          error(
            HTTP_ROUTES_ERROR_CODES.visibilityOverlap,
            `routes[${right}]`,
            `route overlaps routes[${left}] for at least one HTTP method but publicEdge differs`,
          ),
        );
      }
    }
  }
  return diagnostics;
}

function validateRouteShape(value: unknown, field: string, diagnostics: HttpRoutesDiagnostic[]): void {
  if (!isRecord(value)) {
    diagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, field, 'route must be an object'));
    return;
  }
  validateObjectShape(
    value,
    ['match', 'path', 'methods', 'publicEdge', 'provenance'],
    ['match', 'path', 'methods', 'publicEdge', 'provenance'],
    field,
    diagnostics,
  );
  if (typeof value['match'] !== 'string')
    diagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.match`, 'match must be a string'));
  if (typeof value['path'] !== 'string')
    diagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.path`, 'path must be a string'));
  if (!Array.isArray(value['methods']) || value['methods'].some((method) => typeof method !== 'string')) {
    diagnostics.push(
      error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.methods`, 'methods must be an array of strings'),
    );
  }
  if (typeof value['publicEdge'] !== 'boolean')
    diagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.publicEdge`, 'publicEdge must be a boolean'));
  const provenance = value['provenance'];
  if (!isRecord(provenance)) {
    diagnostics.push(error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.provenance`, 'provenance must be an object'));
    return;
  }
  validateObjectShape(
    provenance,
    ['project', 'package', 'sourceKind', 'evidencePath'],
    ['project', 'sourceKind'],
    `${field}.provenance`,
    diagnostics,
  );
  if (typeof provenance['project'] !== 'string')
    diagnostics.push(
      error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.provenance.project`, 'project must be a string'),
    );
  if (provenance['package'] !== undefined && typeof provenance['package'] !== 'string')
    diagnostics.push(
      error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.provenance.package`, 'package must be a string'),
    );
  if (typeof provenance['sourceKind'] !== 'string')
    diagnostics.push(
      error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.provenance.sourceKind`, 'sourceKind must be a string'),
    );
  if (provenance['evidencePath'] !== undefined && typeof provenance['evidencePath'] !== 'string')
    diagnostics.push(
      error(HTTP_ROUTES_ERROR_CODES.parseError, `${field}.provenance.evidencePath`, 'evidencePath must be a string'),
    );
}

function validateObjectShape(
  value: Record<string, unknown>,
  allowed: readonly string[],
  required: readonly string[],
  field: string,
  diagnostics: HttpRoutesDiagnostic[],
): void {
  const allowedSet = new Set(allowed);
  for (const key of Object.keys(value)) {
    if (!allowedSet.has(key))
      diagnostics.push(
        error(
          HTTP_ROUTES_ERROR_CODES.unknownField,
          field ? `${field}.${key}` : key,
          `unknown field ${JSON.stringify(key)}`,
        ),
      );
  }
  for (const key of required) {
    if (!Object.hasOwn(value, key))
      diagnostics.push(
        error(
          HTTP_ROUTES_ERROR_CODES.parseError,
          field ? `${field}.${key}` : key,
          `required field ${JSON.stringify(key)} is missing`,
        ),
      );
  }
}

function validateRoute(route: HttpRoute, field: string): HttpRoutesDiagnostic[] {
  const diagnostics: HttpRoutesDiagnostic[] = [];
  if (!VALID_MATCHES.has(route.match))
    diagnostics.push(
      error(
        HTTP_ROUTES_ERROR_CODES.invalidMatch,
        `${field}.match`,
        `match ${JSON.stringify(route.match)} is not in the v1 set`,
      ),
    );
  else diagnostics.push(...validatePath(route.match, route.path, `${field}.path`));

  if (route.methods.length === 0)
    diagnostics.push(
      error(HTTP_ROUTES_ERROR_CODES.invalidMethod, `${field}.methods`, 'at least one HTTP method is required'),
    );
  const seen = new Set<string>();
  route.methods.forEach((method, index) => {
    if (method !== method.toUpperCase() || !VALID_METHODS.has(method as HttpRouteMethod)) {
      diagnostics.push(
        error(
          HTTP_ROUTES_ERROR_CODES.invalidMethod,
          `${field}.methods[${index}]`,
          `method ${JSON.stringify(method)} is not a canonical v1 HTTP method`,
        ),
      );
    }
    if (seen.has(method))
      diagnostics.push(
        error(
          HTTP_ROUTES_ERROR_CODES.duplicateMethod,
          `${field}.methods[${index}]`,
          `method ${JSON.stringify(method)} appears more than once`,
        ),
      );
    seen.add(method);
  });
  if (route.provenance.project.trim() === '')
    diagnostics.push(
      error(HTTP_ROUTES_ERROR_CODES.missingProvenance, `${field}.provenance.project`, 'provenance.project is required'),
    );
  if (!VALID_SOURCE_KINDS.has(route.provenance.sourceKind))
    diagnostics.push(
      error(
        HTTP_ROUTES_ERROR_CODES.invalidSourceKind,
        `${field}.provenance.sourceKind`,
        `sourceKind ${JSON.stringify(route.provenance.sourceKind)} is not in the v1 set`,
      ),
    );
  const evidence = route.provenance.evidencePath;
  if (evidence !== undefined && evidence !== '') {
    const clean = pathPosix.normalize(evidence).replace(/\/$/, '');
    if (
      evidence.includes('\\') ||
      evidence.startsWith('/') ||
      clean !== evidence ||
      evidence === '..' ||
      evidence.startsWith('../')
    ) {
      diagnostics.push(
        error(
          HTTP_ROUTES_ERROR_CODES.invalidEvidencePath,
          `${field}.provenance.evidencePath`,
          'evidencePath must be a normalized workspace-relative path that does not escape the project',
        ),
      );
    }
  }
  return diagnostics;
}

function validatePath(kind: HttpRouteMatch, routePath: string, field: string): HttpRoutesDiagnostic[] {
  if (routePath === '' || !routePath.startsWith('/'))
    return [error(HTTP_ROUTES_ERROR_CODES.invalidPath, field, "path must be absolute and begin with '/'")];
  if (routePath.includes('//'))
    return [error(HTTP_ROUTES_ERROR_CODES.invalidPath, field, "path must not contain repeated '/' separators")];
  if (/[?#\\*[\]()]/.test(routePath) || routePath.includes('{...'))
    return [
      error(
        HTTP_ROUTES_ERROR_CODES.unsupportedPattern,
        field,
        'wildcards, catch-alls, regular expressions, query strings, fragments, and backslashes are unsupported',
      ),
    ];
  if (routePath.includes(':'))
    return [
      error(
        HTTP_ROUTES_ERROR_CODES.unsupportedPattern,
        field,
        "provider-native ':param' syntax is unsupported; convert named segments to '{param}'",
      ),
    ];
  const escapingError = validateEscaping(routePath);
  if (escapingError) return [error(HTTP_ROUTES_ERROR_CODES.invalidPath, field, escapingError)];

  const segments = routePath.split('/').slice(1);
  if (segments.some((segment) => segment === '.' || segment === '..'))
    return [error(HTTP_ROUTES_ERROR_CODES.invalidPath, field, 'dot segments are not canonical route paths')];
  const unsupportedSegment = error(
    HTTP_ROUTES_ERROR_CODES.unsupportedPattern,
    field,
    "template parameters must occupy one complete segment and use '{name}' syntax",
  );
  let parameterCount = 0;
  let catchAllCount = 0;
  for (const segment of segments) {
    if (!/[{}]/.test(segment)) continue;
    if (
      segment.length < 3 ||
      !segment.startsWith('{') ||
      !segment.endsWith('}') ||
      (segment.match(/{/g)?.length ?? 0) !== 1 ||
      (segment.match(/}/g)?.length ?? 0) !== 1
    ) {
      return [unsupportedSegment];
    }
    const inner = segment.slice(1, -1);
    // A '{name...}' segment is a catch-all: it matches one or more complete
    // segments (slashes included). Its name reuses the single-segment parameter
    // grammar; the trailing "..." marks the catch-all. The standalone '{...'
    // check above still rejects an anonymous '{...}'.
    if (inner.endsWith('...')) {
      if (!PARAMETER_NAME.test(inner.slice(0, -3))) return [unsupportedSegment];
      catchAllCount++;
    } else if (!PARAMETER_NAME.test(inner)) {
      return [unsupportedSegment];
    }
    parameterCount++;
  }
  // At most one catch-all per path. The runtime trie router supports a single
  // unbounded catch-all (optionally before a fixed suffix), never two.
  if (catchAllCount > 1)
    return [
      error(
        HTTP_ROUTES_ERROR_CODES.unsupportedPattern,
        field,
        "a path may contain at most one '{name...}' catch-all segment",
      ),
    ];
  if (kind === 'exact' && parameterCount !== 0)
    return [error(HTTP_ROUTES_ERROR_CODES.unsupportedPattern, field, 'exact paths cannot contain template parameters')];
  if (kind === 'prefix') {
    if (parameterCount !== 0)
      return [
        error(HTTP_ROUTES_ERROR_CODES.unsupportedPattern, field, 'prefix paths cannot contain template parameters'),
      ];
    if (routePath === '/')
      return [
        error(
          HTTP_ROUTES_ERROR_CODES.unsupportedPattern,
          field,
          "the root prefix is forbidden because a static mount must never imply '/*'",
        ),
      ];
    if (!routePath.endsWith('/'))
      return [
        error(
          HTTP_ROUTES_ERROR_CODES.invalidPath,
          field,
          "prefix paths must end in '/' so segment ownership is explicit",
        ),
      ];
  }
  if (kind === 'template' && parameterCount === 0)
    return [
      error(HTTP_ROUTES_ERROR_CODES.invalidPath, field, "template paths must contain at least one '{name}' segment"),
    ];
  return [];
}

function validateEscaping(routePath: string): string | undefined {
  for (let index = 0; index < routePath.length; index++) {
    const code = routePath.charCodeAt(index);
    if (code >= 0x80 || code < 0x21 || code === 0x7f)
      return 'path must use visible ASCII; UTF-8 bytes must be percent-encoded';
    if (code !== 0x25) continue;
    if (index + 2 >= routePath.length || !isUpperHex(routePath[index + 1]) || !isUpperHex(routePath[index + 2]))
      return 'percent escapes must use two uppercase hexadecimal digits';
    const decoded = Number.parseInt(routePath.slice(index + 1, index + 3), 16);
    if (
      decoded === 0x2f ||
      decoded === 0x5c ||
      decoded === 0x25 ||
      decoded === 0x3f ||
      decoded === 0x23 ||
      decoded < 0x20 ||
      decoded === 0x7f
    )
      return 'percent-encoded separators, percent signs, query/fragment markers, and controls are forbidden';
    if (isUnreserved(decoded)) return 'percent-encoded unreserved characters must be written literally';
    index += 2;
  }
  try {
    decodeURIComponent(routePath);
  } catch {
    return 'percent escapes must encode valid UTF-8';
  }
  return undefined;
}

function canonicalProvenance(provenance: HttpRouteProvenance): HttpRouteProvenance {
  return {
    project: provenance.project,
    ...(provenance.package ? { package: provenance.package } : {}),
    sourceKind: provenance.sourceKind,
    ...(provenance.evidencePath ? { evidencePath: provenance.evidencePath } : {}),
  };
}

function computeDigest(routes: readonly HttpRoute[]): string {
  const projection = { protocol: HTTP_ROUTES_PROTOCOL, routes };
  const bytes = `${escapeLikeGo(JSON.stringify(projection, null, 2))}\n`;
  return `sha256:${createHash('sha256').update(bytes, 'utf8').digest('hex')}`;
}

function compareRoutes(a: HttpRoute, b: HttpRoute): number {
  const left = routeSortKey(a);
  const right = routeSortKey(b);
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'));
}

function compareStrings(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function routeSortKey(route: HttpRoute): string {
  const rank = route.match === 'exact' ? '0' : route.match === 'template' ? '1' : '2';
  return [
    semanticPath(route),
    rank,
    route.path,
    route.methods.join(','),
    route.publicEdge ? '1' : '0',
    route.provenance.project,
    route.provenance.package ?? '',
    route.provenance.sourceKind,
    route.provenance.evidencePath ?? '',
  ].join('\0');
}

function semanticPath(route: Pick<HttpRoute, 'match' | 'path'>): string {
  if (route.match !== 'template') return route.path;
  return route.path
    .split('/')
    .map((segment) =>
      // A catch-all normalizes to a token distinct from a single-segment
      // parameter so '/x/{id}' and '/x/{id...}' are never duplicates.
      isCatchAllSegment(segment) ? '{...}' : isParameterSegment(segment) ? '{}' : segment,
    )
    .join('/');
}

function methodsOverlap(a: readonly string[], b: readonly string[]): boolean {
  const left = new Set(a);
  return b.some((method) => left.has(method));
}

function sameMatchLanguage(a: HttpRoute, b: HttpRoute): boolean {
  return a.match === b.match && semanticPath(a) === semanticPath(b);
}

function pathsOverlap(a: HttpRoute, b: HttpRoute): boolean {
  // A catch-all can absorb a variable number of segments, so it cannot be
  // compared with the fixed-length routines below. Route any pair involving one
  // to the segment-boundary analysis, which over-approximates overlap so a
  // public catch-all can never silently coexist with a private route it matches.
  if (isCatchAll(a) || isCatchAll(b)) return patternsOverlap(toPattern(a), toPattern(b));
  if (a.match === 'prefix' && b.match === 'prefix') return a.path.startsWith(b.path) || b.path.startsWith(a.path);
  if (a.match === 'prefix') return prefixOverlaps(a.path, b);
  if (b.match === 'prefix') return prefixOverlaps(b.path, a);
  return fixedPatternsOverlap(a, b);
}

/**
 * A segment-boundary view of a route used for catch-all overlap. A fixed route
 * stores all its segments in prefix; a catch-all (or a '/'-mount) stores the
 * segments before the catch-all in prefix and those after it in suffix, with
 * hasCatchAll marking the variable, one-or-more-segment gap.
 */
interface PathPattern {
  prefix: string[];
  suffix: string[];
  hasCatchAll: boolean;
}

function toPattern(route: HttpRoute): PathPattern {
  if (route.match === 'prefix') {
    // A '/'-terminated prefix owns its segments plus one or more further
    // segments — the same reachable set as a trailing '{rest...}' catch-all.
    return { prefix: route.path.replace(/\/$/, '').split('/').slice(1), suffix: [], hasCatchAll: true };
  }
  const segments = route.path.split('/').slice(1);
  const index = segments.findIndex((segment) => isCatchAllSegment(segment));
  if (index >= 0) return { prefix: segments.slice(0, index), suffix: segments.slice(index + 1), hasCatchAll: true };
  return { prefix: segments, suffix: [], hasCatchAll: false };
}

function patternsOverlap(a: PathPattern, b: PathPattern): boolean {
  if (a.hasCatchAll && b.hasCatchAll) {
    // Both have a variable gap: for a long-enough path the gaps never collide,
    // so overlap turns on whether the overlapping fixed heads and fixed tails
    // are mutually satisfiable. Positions only one pattern constrains fall in
    // the other's gap and impose nothing.
    return sharedSegmentsCompatible(a.prefix, b.prefix, false) && sharedSegmentsCompatible(a.suffix, b.suffix, true);
  }
  if (a.hasCatchAll) return fixedMatchesCatchAll(b.prefix, a);
  if (b.hasCatchAll) return fixedMatchesCatchAll(a.prefix, b);
  if (a.prefix.length !== b.prefix.length) return false;
  return sharedSegmentsCompatible(a.prefix, b.prefix, false);
}

/**
 * Report whether a fixed-length segment list can satisfy a catch-all pattern: it
 * must be long enough for the catch-all to absorb at least one segment, and its
 * bounding segments must be compatible with the catch-all's fixed prefix/suffix.
 */
function fixedMatchesCatchAll(fixed: string[], catchPattern: PathPattern): boolean {
  if (fixed.length < catchPattern.prefix.length + catchPattern.suffix.length + 1) return false;
  if (!catchPattern.prefix.every((segment, index) => segmentsCompatible(fixed[index], segment))) return false;
  const base = fixed.length - catchPattern.suffix.length;
  return catchPattern.suffix.every((segment, index) => segmentsCompatible(fixed[base + index], segment));
}

/**
 * Compare the overlapping ends of two segment lists. With fromEnd it aligns them
 * by their tails (a shared suffix); otherwise by their heads (a shared prefix).
 * Positions only one list reaches are absorbed by the counterpart's catch-all
 * and never force disjointness.
 */
function sharedSegmentsCompatible(a: string[], b: string[], fromEnd: boolean): boolean {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) {
    const ai = fromEnd ? a.length - 1 - i : i;
    const bi = fromEnd ? b.length - 1 - i : i;
    if (!segmentsCompatible(a[ai], b[bi])) return false;
  }
  return true;
}

/**
 * Report whether two fixed segments can match a common concrete segment. A
 * single-segment parameter matches any non-empty segment; two literals must be
 * equal.
 */
function segmentsCompatible(a: string, b: string): boolean {
  if (isParameterSegment(a) || isParameterSegment(b)) return true;
  return a === b;
}

function isCatchAllSegment(segment: string | undefined): boolean {
  if (segment === undefined || segment.length < 3 || !segment.startsWith('{') || !segment.endsWith('}')) return false;
  const inner = segment.slice(1, -1);
  return inner.endsWith('...') && PARAMETER_NAME.test(inner.slice(0, -3));
}

function isCatchAll(route: HttpRoute): boolean {
  if (route.match !== 'template') return false;
  return route.path
    .split('/')
    .slice(1)
    .some((segment) => isCatchAllSegment(segment));
}

function prefixOverlaps(prefix: string, other: HttpRoute): boolean {
  if (other.match === 'exact') return other.path.startsWith(prefix);
  const prefixSegments = prefix.slice(0, -1).split('/').slice(1);
  const otherSegments = other.path.split('/').slice(1);
  if (otherSegments.length <= prefixSegments.length) return false;
  return prefixSegments.every(
    (segment, index) => isParameterSegment(otherSegments[index]) || otherSegments[index] === segment,
  );
}

function fixedPatternsOverlap(a: HttpRoute, b: HttpRoute): boolean {
  const left = a.path.split('/');
  const right = b.path.split('/');
  if (left.length !== right.length) return false;
  return left.every((segment, index) => {
    const other = right[index];
    return (
      segment === other ||
      (isParameterSegment(segment) && other !== '') ||
      (isParameterSegment(other) && segment !== '')
    );
  });
}

function isParameterSegment(segment: string | undefined): boolean {
  return segment !== undefined && segment.length >= 3 && segment.startsWith('{') && segment.endsWith('}');
}

function isUpperHex(value: string): boolean {
  return /^[0-9A-F]$/.test(value);
}

function isUnreserved(value: number): boolean {
  return (
    (value >= 0x61 && value <= 0x7a) ||
    (value >= 0x41 && value <= 0x5a) ||
    (value >= 0x30 && value <= 0x39) ||
    '-._~'.includes(String.fromCharCode(value))
  );
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** Match encoding/json's default HTML and JavaScript-separator escaping. */
function escapeLikeGo(json: string): string {
  return json.replace(/[<>&\u2028\u2029]/g, (character) => {
    switch (character) {
      case '<':
        return '\\u003c';
      case '>':
        return '\\u003e';
      case '&':
        return '\\u0026';
      case '\u2028':
        return '\\u2028';
      case '\u2029':
        return '\\u2029';
      default:
        return character;
    }
  });
}

function error(code: string, field: string, message: string): HttpRoutesDiagnostic {
  return { severity: 'error', code, message, ...(field ? { field } : {}) };
}
