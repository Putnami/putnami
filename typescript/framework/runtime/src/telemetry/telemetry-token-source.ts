/**
 * The telemetry token source: the credential every push to an OTLP collector
 * carries.
 *
 * The framework sends telemetry, but the credential a collector admits belongs
 * to the platform that runs the collector. A runtime package, such as
 * `@putnami/cloud/runtime`, registers one source per process; the telemetry
 * plugin asks it for a bearer before each push. The source owns its cache, so
 * an expiring token is renewed without a restart.
 */

/**
 * Returns the bearer token for a push to the collector at `endpoint`, or
 * `undefined` to send no `Authorization` header. A rejection means no
 * credential could be obtained: the push is dropped.
 */
export type TelemetryTokenSource = (endpoint: string) => Promise<string | undefined>;

let registeredSource: TelemetryTokenSource | undefined;

/**
 * Registers the process-wide telemetry token source. The last registration
 * wins, so registering the same source twice is harmless.
 */
export function registerTelemetryTokenSource(source: TelemetryTokenSource): void {
  registeredSource = source;
}

/** The registered telemetry token source, or `undefined` when none is. */
export function getTelemetryTokenSource(): TelemetryTokenSource | undefined {
  return registeredSource;
}

/** Removes the registered telemetry token source. Reached through `@putnami/runtime/testing`. */
export function resetTelemetryTokenSourceForTest(): void {
  registeredSource = undefined;
}
