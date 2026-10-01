/**
 * Runtime content-overlay plugin.
 *
 * Wires the self-update overlay into the running site:
 *
 * - a per-request hook that fires the TTL-gated, single-flight newness check
 *   (fire-and-forget — never on the serving critical path);
 * - an overlay-aware `GET /search/index.json` route. The StaticPlugin also
 *   registers that exact path from the baked build output; this dynamic route
 *   wins the router's content-negotiation scoring by declaring an exact
 *   Accept match (`application/json` / `*​/*` score 1 vs the accept-less
 *   static route's 0.5 wildcard score), and it is additionally registered
 *   BEFORE the staticFiles plugin so an accept-tie resolves to it as well;
 * - a self-guarded `POST /api/content/refresh` endpoint. The site deploys
 *   with `platformAuth: disabled`, so the route enforces auth in-handler,
 *   fail-closed: OIDC bearer verified against the configured issuer/audience
 *   plus a subject/email allowlist (mirrors the events push-receiver) — any
 *   missing/invalid/not-allowlisted credential is the same 401. On success it
 *   ingests the current channel head immediately, bypassing the TTL.
 *
 * Config section `contentOverlay` holds behavior knobs only; the bundle
 * identity (package/registry/name) comes from the committed
 * `content.lock.json`, whose digest stays the baked fallback pin.
 */
import {
  HttpPlugin,
  HttpResponse,
  json,
  type Module,
  OAuthService,
  type Plugin,
  unauthorized,
} from '@putnami/application';
import {
  ArrayOf,
  Config,
  Default,
  Desc,
  type InferConfig,
  Int,
  Optional,
  useConfig,
  useLogger,
} from '@putnami/runtime';
import { fileExists, getProjectRoot, joinPath } from '@putnami/utils';
import type { BlobFetch } from '../lib/content/ingest';
import type { LockBundle } from '../lib/content/lock';
import { manifestPathFor, readMaterializedMounts } from '../lib/content/materialize';
import { type ConvergeOptions, forceCheckAndConverge, maybeCheckAndConverge } from '../lib/content/newness';
import { activeContentDigest, getActiveDocsRoot } from '../lib/content/overlay';
import type { ChannelPointer } from '../lib/content/registry';
import { getBakedDocsRoot } from '../lib/docs/navigation.server';

export const ContentOverlayConfig = Config('contentOverlay', {
  enabled: Default(Boolean, true),
  /** Minimum interval between channel-pointer polls (SWR newness check). */
  checkTtlMs: Default(Int, 300_000),
  /** Max accepted bundle blob size (tmpfs/memory footprint guard). */
  sizeCapBytes: Default(Int, 64 * 1024 * 1024),
  refresh: Optional(
    Desc('OIDC guard for POST /api/content/refresh (unset ⇒ endpoint always 401s)', {
      issuer: Optional(String),
      audience: Optional(String),
      allowedSubjects: Default(ArrayOf(String), []),
    }),
  ),
});

type ContentOverlayConfigType = InferConfig<typeof ContentOverlayConfig>;
type RefreshConfig = ContentOverlayConfigType['refresh'];

/** Verified OIDC claims, or undefined when verification fails. */
export type RefreshTokenVerifier = (token: string) => Promise<Record<string, unknown> | undefined>;

/** Test/dev seams; production uses config + registry/OIDC defaults. */
export interface ContentOverlayHooks {
  lockPath?: string;
  bakedDocsRoot?: string;
  bakedMounts?: string[];
  versionsDir?: string;
  fetchPointer?: (bundle: LockBundle) => Promise<ChannelPointer>;
  fetchBlob?: BlobFetch;
  onSwap?: () => Promise<void>;
  verifyToken?: RefreshTokenVerifier;
  now?: () => number;
  /** Programmatic config defaults (YAML/env still take precedence). */
  confInit?: Partial<ContentOverlayConfigType>;
}

/**
 * Default refresh verifier: a standalone OAuthService (the site registers no
 * oauth plugin) discovering the JWKS from the configured issuer, verifying
 * signature + issuer + audience. Mirrors the events push-receiver model.
 */
function defaultVerifier(refresh: RefreshConfig): RefreshTokenVerifier {
  let service: OAuthService | undefined;
  return async (token) => {
    if (!refresh?.issuer || !refresh.audience) return undefined;
    if (!service) {
      service = new OAuthService();
      service.confInit = {
        issuer: refresh.issuer,
        discoveryUri: `${refresh.issuer.replace(/\/+$/, '')}/.well-known/openid-configuration`,
      };
    }
    return service.verify<Record<string, unknown>>(token, { issuer: refresh.issuer, audience: refresh.audience });
  };
}

function bearerToken(headers: Headers): string | undefined {
  const authorization = headers.get('authorization');
  if (!authorization?.startsWith('Bearer ')) return undefined;
  return authorization.slice('Bearer '.length).trim() || undefined;
}

/** Outcome of the refresh authorization decision (case-tested in isolation). */
export type RefreshAuth =
  | { ok: true; subject: string }
  | { ok: false; reason: 'unconfigured' | 'bad-token' | 'not-allowlisted' };

/**
 * Decide whether a refresh request is authorized, fail-closed. Extracted from
 * the handler so the full 401 matrix is testable without an app context: a
 * missing bearer, an unconfigured guard, a token that fails verification, and
 * a verified-but-not-allowlisted subject each map to a distinct `reason` the
 * caller logs, but all become the same 401 (no oracle about which check
 * failed). A subject is accepted by verified email or by `sub`.
 */
export async function authorizeRefresh(
  headers: Headers,
  refresh: RefreshConfig,
  verifyToken: RefreshTokenVerifier,
): Promise<RefreshAuth> {
  const token = bearerToken(headers);
  const allowlist = new Set(refresh?.allowedSubjects ?? []);
  if (!token || !refresh?.issuer || !refresh.audience || allowlist.size === 0) {
    return { ok: false, reason: 'unconfigured' };
  }
  const claims = await verifyToken(token);
  if (!claims) return { ok: false, reason: 'bad-token' };
  const email = typeof claims['email'] === 'string' ? claims['email'] : undefined;
  const sub = typeof claims['sub'] === 'string' ? claims['sub'] : undefined;
  if (email !== undefined && claims['email_verified'] !== false && allowlist.has(email)) {
    return { ok: true, subject: email };
  }
  if (sub !== undefined && allowlist.has(sub)) return { ok: true, subject: sub };
  return { ok: false, reason: 'not-allowlisted' };
}

class ContentOverlayPlugin implements Plugin {
  constructor(private hooks: ContentOverlayHooks) {}

  async warmup(app: Module): Promise<void> {
    const cfg = useConfig(ContentOverlayConfig, { confInit: this.hooks.confInit });
    if (!cfg.enabled) return;

    const logger = useLogger('putnami.dev');
    const log = (message: string) => logger.info(message);
    const projectRoot = getProjectRoot();
    const bakedDocsRoot = this.hooks.bakedDocsRoot ?? getBakedDocsRoot();
    // Bundle mounts baked into the image (empty under the v1 empty lock). Read
    // once at warmup from the cache-restored generate sidecar so a dropped or
    // renamed mount is pruned on ingest instead of lingering as stale docs.
    const bakedMounts = this.hooks.bakedMounts ?? readMaterializedMounts(manifestPathFor(projectRoot));
    const convergeOptions: ConvergeOptions = {
      lockPath: this.hooks.lockPath ?? joinPath(projectRoot, 'content.lock.json'),
      bakedDocsRoot,
      bakedMounts,
      checkTtlMs: cfg.checkTtlMs,
      sizeCapBytes: cfg.sizeCapBytes,
      versionsDir: this.hooks.versionsDir,
      fetchPointer: this.hooks.fetchPointer,
      fetchBlob: this.hooks.fetchBlob,
      log,
      onSwap: this.hooks.onSwap,
      now: this.hooks.now,
    };

    const httpPlugin = await app.ensurePlugin(HttpPlugin);

    // Newness trigger: TTL-gated + single-flight inside maybeCheckAndConverge,
    // fire-and-forget here — the request proceeds without ever awaiting it.
    httpPlugin.use(async (_ctx, next) => {
      maybeCheckAndConverge(convergeOptions);
      return next();
    });

    // Overlay-aware search index (accept declared so it outranks the baked
    // StaticPlugin registration of the same exact path — see module header).
    httpPlugin.route('GET', '/search/index.json', () => this.serveSearchIndex(bakedDocsRoot, projectRoot), {
      accept: ['application/json', '*/*'],
    });

    // Self-guarded refresh endpoint (fail-closed 401 on any auth defect).
    // `accept` is declared so JSON clients match: the router filters a route
    // with no declared accept out of an `Accept: application/json` request
    // (scoreAccept → 0), which would otherwise make this endpoint unreachable.
    const verifyToken = this.hooks.verifyToken ?? defaultVerifier(cfg.refresh);
    httpPlugin.route(
      'POST',
      '/api/content/refresh',
      async (ctx) => this.handleRefresh(ctx.headers, cfg.refresh, verifyToken, convergeOptions, logger),
      { accept: ['application/json', '*/*'] },
    );
  }

  private serveSearchIndex(bakedDocsRoot: string, projectRoot: string): HttpResponse {
    const active = getActiveDocsRoot(bakedDocsRoot);
    const candidates: string[] = [];
    if (active !== bakedDocsRoot) {
      // Version layout: <versionsDir>/<digest>/{docs,search/index.json}.
      candidates.push(joinPath(active, '..', 'search', 'index.json'));
    }
    candidates.push(joinPath(projectRoot, '.gen', 'public', 'search', 'index.json'));
    const path = candidates.find((candidate) => fileExists(candidate));
    if (!path) {
      return new HttpResponse('Not found', { status: 404 });
    }
    return new HttpResponse(Bun.file(path), {
      headers: {
        'Content-Type': 'application/json',
        // Short-lived: overlay swaps must reach clients without waiting out a
        // month-long static cache header.
        'Cache-Control': 'public, max-age=300',
      },
    });
  }

  private async handleRefresh(
    headers: Headers,
    refresh: RefreshConfig,
    verifyToken: RefreshTokenVerifier,
    convergeOptions: ConvergeOptions,
    logger: ReturnType<typeof useLogger>,
  ): Promise<HttpResponse> {
    // Fail-closed, and uniformly so: a missing guard config, missing/invalid
    // bearer, and a non-allowlisted subject are all the same 401 (no oracle
    // about which check failed) — only the log records which check tripped.
    const auth = await authorizeRefresh(headers, refresh, verifyToken);
    if (!auth.ok) {
      logger.warn(`content overlay: refresh rejected (${auth.reason})`);
      return unauthorized();
    }

    const outcome = await forceCheckAndConverge(convergeOptions);
    logger.info(
      `content overlay: refresh by ${auth.subject}: ${outcome?.outcome ?? 'no-bundle'} ` +
        `(active digest ${outcome?.activeDigest ?? activeContentDigest() ?? 'baked'})`,
    );
    return json({
      outcome: outcome?.outcome ?? 'no-bundle',
      activeDigest: outcome?.activeDigest ?? activeContentDigest(),
      ...(outcome?.reason !== undefined ? { reason: outcome.reason } : {}),
    });
  }
}

export function contentOverlay(hooks: ContentOverlayHooks = {}): ContentOverlayPlugin {
  return new ContentOverlayPlugin(hooks);
}
