/**
 * Telemetry from the environment — the standard OpenTelemetry variables.
 *
 * `OTEL_EXPORTER_OTLP_ENDPOINT` turns telemetry on, `OTEL_SDK_DISABLED=true`
 * turns it off, and the service identity comes from `OTEL_SERVICE_NAME` and
 * `OTEL_RESOURCE_ATTRIBUTES`. `application()` adds the plugin this module
 * builds when the composition holds no `telemetry()` of its own.
 */

import { ATTR_SERVICE_INSTANCE_ID } from './otlp';

/** Collector base URL. Telemetry from the environment is on only when it is set. */
export const ENV_OTLP_ENDPOINT = 'OTEL_EXPORTER_OTLP_ENDPOINT';
/** `true` turns telemetry from the environment off, whatever the endpoint says. */
export const ENV_SDK_DISABLED = 'OTEL_SDK_DISABLED';
/** The service name every pushed resource carries. */
export const ENV_SERVICE_NAME = 'OTEL_SERVICE_NAME';
/** Comma-separated key=value pairs every pushed resource carries. */
export const ENV_RESOURCE_ATTRIBUTES = 'OTEL_RESOURCE_ATTRIBUTES';

type Environment = Readonly<Record<string, string | undefined>>;

let instanceId: string | undefined;

/**
 * The `service.instance.id` of this process: a random UUID drawn once, the
 * same for every push the process makes.
 */
export function processInstanceId(): string {
  instanceId ??= crypto.randomUUID();
  return instanceId;
}

/**
 * The attributes with a `service.instance.id`: the non-empty one they carry,
 * else the process's own.
 */
export function withProcessInstance(attributes: Readonly<Record<string, string>>): Record<string, string> {
  return {
    ...attributes,
    [ATTR_SERVICE_INSTANCE_ID]: attributes[ATTR_SERVICE_INSTANCE_ID] || processInstanceId(),
  };
}

/**
 * The collector endpoint the environment names, without trailing slashes, or
 * `undefined` when it names none or turns telemetry off.
 */
export function environmentEndpoint(env: Environment = process.env): string | undefined {
  if (env[ENV_SDK_DISABLED]?.trim().toLowerCase() === 'true') return undefined;
  const endpoint = env[ENV_OTLP_ENDPOINT]?.trim().replace(/\/+$/, '');
  return endpoint ? endpoint : undefined;
}

/** The service identity the environment gives every pushed resource. */
export interface EnvironmentResource {
  /** `OTEL_SERVICE_NAME`, when set. */
  serviceName?: string;
  /** The pairs of `OTEL_RESOURCE_ATTRIBUTES`. */
  attributes: Record<string, string>;
}

/** Read `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`. */
export function environmentResource(env: Environment = process.env): EnvironmentResource {
  const serviceName = env[ENV_SERVICE_NAME]?.trim();
  return {
    ...(serviceName ? { serviceName } : {}),
    attributes: parseResourceAttributes(env[ENV_RESOURCE_ATTRIBUTES]),
  };
}

/**
 * Parse `OTEL_RESOURCE_ATTRIBUTES`: comma-separated `key=value` pairs whose
 * keys and values may be percent-encoded. A pair without `=`, with an empty
 * key or with a malformed encoding is skipped; a later key wins.
 */
export function parseResourceAttributes(raw: string | undefined): Record<string, string> {
  const attributes: Record<string, string> = {};
  if (!raw) return attributes;
  for (const pair of raw.split(',')) {
    const separator = pair.indexOf('=');
    if (separator <= 0) continue;
    try {
      const key = decodeURIComponent(pair.slice(0, separator).trim());
      const value = decodeURIComponent(pair.slice(separator + 1).trim());
      if (key) attributes[key] = value;
    } catch {
      // A malformed percent-encoding names no attribute.
    }
  }
  return attributes;
}
