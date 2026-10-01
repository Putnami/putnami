import { track } from '@putnami/analytics';
import { Desc, endpoint, HttpResponse, type HttpRequestContext, Optional, Pattern } from '@putnami/application';
import { BadRequestException, useContext } from '@putnami/runtime';

const DEFAULT_REGISTRY_URL = 'https://put.putnami.dev';

const ARTIFACT_PARAM_SCHEMA = {
  artifact: Desc('Artifact identifier to resolve (for example: putnami).', Pattern(/^[A-Za-z0-9][A-Za-z0-9._-]*$/)),
};

const DOWNLOAD_QUERY_SCHEMA = {
  version: Desc(
    'Version selector (latest/canary/dev or explicit semver like v1.2.3). Defaults to latest when omitted.',
    Optional(String),
  ),
  platform: Desc(
    'Target OS. Accepted values: linux, darwin. Aliases: macos, mac.',
    Pattern(/^(linux|darwin|macos|mac)$/i),
  ),
  target: Desc(
    'Target architecture. Accepted values: x64, arm64. Aliases: amd64, x86_64, aarch64.',
    Pattern(/^(x64|arm64|amd64|x86_64|aarch64)$/i),
  ),
};

const ERROR_RESPONSE_SCHEMA = {
  statusCode: Number,
  message: String,
  error: String,
};

type Platform = 'linux' | 'darwin';
type Target = 'x64' | 'arm64';

function normalizePlatform(value: string | undefined): Platform | undefined {
  if (!value) return undefined;
  const v = value.trim().toLowerCase();
  if (v === 'linux') return 'linux';
  if (v === 'darwin' || v === 'macos' || v === 'mac') return 'darwin';
  return undefined;
}

function normalizeTarget(value: string | undefined): Target | undefined {
  if (!value) return undefined;
  const v = value.trim().toLowerCase();
  if (v === 'x64' || v === 'amd64' || v === 'x86_64') return 'x64';
  if (v === 'arm64' || v === 'aarch64') return 'arm64';
  return undefined;
}

/**
 * Derive registry namespace/package from an artifact identifier.
 * "putnami" → ("putnami", "cli")
 * "putnami-go" → ("putnami", "go")
 */
function registryRef(artifact: string): { ns: string; pkg: string } {
  if (artifact === 'putnami') return { ns: 'putnami', pkg: 'cli' };
  if (artifact.startsWith('putnami-')) return { ns: 'putnami', pkg: artifact.slice('putnami-'.length) };
  return { ns: 'putnami', pkg: artifact };
}

export default endpoint()
  .description('Resolve a Putnami CLI artifact to the registry download endpoint.')
  .params(ARTIFACT_PARAM_SCHEMA)
  .query(DOWNLOAD_QUERY_SCHEMA)
  .response(302, 'Temporary redirect to the registry download endpoint')
  .throws(400, 'Invalid request parameters', ERROR_RESPONSE_SCHEMA)
  .cache({ maxAge: 60, sMaxAge: 60, staleWhileRevalidate: 60 })
  .handle(async (ctx) => {
    const { artifact } = ctx.params;
    const query = ctx.queryParams();

    const platform = normalizePlatform(query.platform);
    const target = normalizeTarget(query.target);

    if (!platform || !target) {
      throw new BadRequestException(
        "Missing or invalid platform/target. Use '?platform=linux|darwin&target=x64|arm64'.",
      );
    }

    const channel = (query.version ?? 'latest').trim() || 'latest';
    const registryUrl = (process.env['PUTNAMI_REGISTRY_URL'] || DEFAULT_REGISTRY_URL).replace(/\/+$/, '');
    const { ns, pkg } = registryRef(artifact);

    const location = `${registryUrl}/${ns}/${pkg}/download?channel=${encodeURIComponent(channel)}&os=${platform}&arch=${target}`;
    // A redirect renders no page, so without this the route is invisible to
    // measurement. What the row counts is exact and narrower than "downloads":
    // one artifact resolution through *this* redirect. `install.sh` fetches
    // from the registry directly, so the common install never appears here,
    // and the `Cache-Control` above lets a browser or a shared cache answer a
    // repeat within 60 seconds without reaching us. Read it as documentation
    // demand per platform, not as an install count.
    //
    // `endpoint()` narrows its own ctx, so the request context comes from the
    // async context — the same call the web action handler makes. Nothing is
    // awaited against the database here: the row is queued.
    await track(useContext<HttpRequestContext>(), 'cli_download', { artifact, platform, target, channel });
    return HttpResponse.redirect(location, 302);
  });
