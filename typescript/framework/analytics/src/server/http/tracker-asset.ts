import { file } from 'bun';
import { readdirSync } from 'node:fs';
import { type HttpPlugin, HttpResponse, PutnamiConfig } from '@putnami/application';
import { useConfig, useLogger } from '@putnami/runtime';
import { fileExists, getProjectRoot, joinPath } from '@putnami/utils';
import type { AnalyticsConfigValues } from '../analytics.config';

const MISSING_BUNDLE_WARNING =
  'analytics: tracker bundle not found; run `putnami build` — collecting server-side page views only';

/**
 * Registers the immutable, gzipped tracker bundle produced by the pre-build
 * hook under `.gen/<publicFolder>/analytics/`.
 *
 * Mirrors the (unexported) script endpoint helper of `@putnami/web`. The
 * warning is logged once at warmup, never per request.
 *
 * @param httpPlugin - The HTTP plugin the route is registered on.
 * @param _config - The resolved analytics configuration (reserved for the plugin seam).
 * @returns The public route of the tracker, or `undefined` when no bundle exists.
 */
export function registerTrackerAsset(httpPlugin: HttpPlugin, _config: AnalyticsConfigValues): string | undefined {
  const putnami = useConfig(PutnamiConfig);
  const outdir = putnami.assetsDir ?? joinPath(getProjectRoot(), '.gen', putnami.publicFolder);
  const dir = joinPath(outdir, 'analytics');
  if (!fileExists(dir)) {
    useLogger('@putnami/analytics').warn(MISSING_BUNDLE_WARNING);
    return undefined;
  }
  const gz = readdirSync(dir)
    .filter((f) => /^analytics\.[A-Za-z0-9]+\.js\.gz$/.test(f))
    .sort()[0];
  if (!gz) {
    useLogger('@putnami/analytics').warn(MISSING_BUNDLE_WARNING);
    return undefined;
  }
  const route = `/analytics/${gz.replace(/\.gz$/, '')}`;
  const headers: Record<string, string> = {
    'Cache-Control': 'public, max-age=31536000, immutable',
    'Content-Type': 'application/javascript',
    'Content-Encoding': 'gzip',
    'Content-Disposition': `filename="${route.substring(1)}"`,
  };
  const filePath = joinPath(dir, gz);
  httpPlugin.get(route, () => new HttpResponse(file(filePath), { headers }));
  return route;
}
