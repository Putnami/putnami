import type { TelemetryTokenSource } from '@putnami/runtime';
import { type GoogleEventServerTokenSourceOptions, googleEventServerTokenSource } from './event-server-destination';

/** Options of {@link googleTelemetryTokenSource}. Tests inject `fetch`, `now` and `env`. */
export interface GoogleTelemetryTokenSourceOptions extends GoogleEventServerTokenSourceOptions {
  /** The environment that says whether the process runs on Google Cloud. Default: `process.env`. */
  env?: Readonly<Record<string, string | undefined>>;
}

/**
 * The telemetry token source of a Putnami Cloud workload.
 *
 * The workspace collector is a Cloud Run service that admits a Google ID token
 * whose audience is its own URL. On Google Cloud (`K_SERVICE` or
 * `GOOGLE_CLOUD_PROJECT` set), the source asks the metadata server for that
 * token, with the collector endpoint as the audience, and keeps it until
 * shortly before it expires. Elsewhere it supplies no token.
 */
export function googleTelemetryTokenSource(options: GoogleTelemetryTokenSourceOptions = {}): TelemetryTokenSource {
  let google: ReturnType<typeof googleEventServerTokenSource> | undefined;
  return async (endpoint) => {
    const env = options.env ?? process.env;
    if (!env['K_SERVICE'] && !env['GOOGLE_CLOUD_PROJECT']) return undefined;
    google ??= googleEventServerTokenSource(options);
    return google({ audience: endpoint, signal: new AbortController().signal });
  };
}

let cloudSource = googleTelemetryTokenSource();

/** The process-wide source `register` installs. */
export const cloudTelemetryTokenSource: TelemetryTokenSource = (endpoint) => cloudSource(endpoint);

/** Drops the cached collector tokens of {@link cloudTelemetryTokenSource}. A config-loader reset hook. */
export function resetTelemetryTokenCacheForTest(): void {
  cloudSource = googleTelemetryTokenSource();
}
