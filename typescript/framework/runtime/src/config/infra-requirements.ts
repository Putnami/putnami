import { randomBytes } from 'node:crypto';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { robustRemoveSync, robustRenameSync } from '../robustio';
import { isNestedSchema, isSchemaDescriptor, type SchemaDefinition } from '../schema';
import type { ConfigDefinition } from './config';
import { getRegisteredConfigDefinitions } from './config-token';

// Per-project infra-requirements protocol. Mirrors the Go protocol in
// protocols/infra. The framework writes its contribution to
// the scratch fragment the TypeScript generator syncs into committed
// infra/requirements.json — see the schema for the wire shape.
const INFRA_PROTOCOL_VERSION = 2;
const INFRA_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-infra.json';

// Infra secret-name grammar, from schemas/infra.json: letters, digits, '-',
// '_', '.', '/'; 1–64 chars. A secret is named by the config key the
// application reads it at (protocols/infra ADR 0004).
const SECRET_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_./-]{0,63}$/;

/**
 * Returns the secret name of a sensitive config field: its config key, the
 * dot-path the application reads it at (`integrations.stripe.apiKey`). A path
 * the secret-name grammar rejects (non-ASCII, illegal characters, or longer
 * than 64 chars) throws, so the developer sees a clear failure at generate
 * time instead of a requirement the deployer refuses.
 */
function secretName(fieldPath: string): string {
  if (!SECRET_NAME_RE.test(fieldPath)) {
    throw new Error(
      `Cannot name the secret of sensitive config field "${fieldPath}": ` +
        `it violates the infra secret-name grammar ${SECRET_NAME_RE.source}. ` +
        `Rename the field or config path to letters, digits, '-', '_', '.', '/' (max 64 chars).`,
    );
  }
  return fieldPath;
}

// Per-producer scratch slug. Each framework producer owns one file at
// <project>/.gen/infra/<slug>.json; the TypeScript generator syncs all
// fragments into committed infra/requirements.json.
const SIDECAR_SLUG = 'secrets';
const SIDECAR_DIR = join('.gen', 'infra');
const SIDECAR_FILENAME = `${SIDECAR_SLUG}.json`;

/**
 * Per-project infrastructure requirements manifest, conforming to
 * https://putnami.dev/schemas/putnami-infra.json. Only the `secrets` slot is
 * populated by config-schema extraction; other resource kinds are sourced by
 * their own framework generators.
 */
export interface InfraRequirementsManifest {
  $schema: string;
  protocolVersion: number;
  secrets?: string[];
}

/**
 * Walks a config schema, collecting the secret name of every field marked
 * `Sensitive`: the dot-path from the config block down to the field (e.g.
 * `database.password`, `integrations.stripe.apiKey`) — see
 * {@link secretName}. A sensitive object is emitted as a single secret; its
 * children are not walked.
 */
function collectFromSchema(schema: SchemaDefinition, prefix: string, out: Set<string>): void {
  for (const [key, prop] of Object.entries(schema)) {
    const path = prefix ? `${prefix}.${key}` : key;

    if (isSchemaDescriptor(prop)) {
      if (prop.sensitive) {
        out.add(secretName(path));
        continue;
      }
      // A Desc()-wrapped nested object carries its shape on `schema`.
      if (prop.baseType === 'object' && prop.schema) {
        collectFromSchema(prop.schema, path, out);
      }
      continue;
    }

    if (isNestedSchema(prop)) {
      collectFromSchema(prop, path, out);
    }
  }
}

/**
 * Collects the secret names of every sensitive field across the given config
 * definitions. Names are deduplicated and sorted so the emitted manifest is
 * deterministic and idempotent for a given input.
 */
export function collectSecretNames(configs: ConfigDefinition[]): string[] {
  const names = new Set<string>();
  for (const config of configs) {
    collectFromSchema(config.schema, config.path, names);
  }
  return [...names].sort();
}

/**
 * Builds the per-project infra-requirements manifest from a set of config
 * definitions, or returns `undefined` when no sensitive fields are declared
 * (nothing to contribute).
 */
export function buildInfraRequirements(configs: ConfigDefinition[]): InfraRequirementsManifest | undefined {
  const secrets = collectSecretNames(configs);
  if (secrets.length === 0) {
    return undefined;
  }
  return {
    $schema: INFRA_SCHEMA_URL,
    protocolVersion: INFRA_PROTOCOL_VERSION,
    secrets,
  };
}

/**
 * Emits the framework-generated per-project infra scratch fragment for a project.
 *
 * Walks the project's config schema (defaulting to every config registered via
 * `configToken()`), and writes `<projectPath>/.gen/infra/secrets.json` with
 * the secrets derived from its `Sensitive` fields. The file is written
 * atomically (write a uniquely-named temp file in the target directory, then
 * rename) as required by the protocol. The same generate task may run
 * concurrently for the build and test pipelines, so each writer uses its own
 * temp file: a shared temp name would let one writer's rename consume the
 * other's temp file, making the second rename throw `ENOENT`. Since the content
 * is idempotent for a given input, whichever rename lands last wins harmlessly.
 *
 * When the project declares no sensitive fields, any scratch fragment left by a
 * prior build is removed so the next generator sync cannot commit stale
 * secrets, and `undefined` is returned. Otherwise the written path is returned.
 */
export function emitInfraRequirements(projectPath: string, configs?: ConfigDefinition[]): string | undefined {
  const definitions = configs ?? getRegisteredConfigDefinitions();
  const manifest = buildInfraRequirements(definitions);

  const dir = join(projectPath, SIDECAR_DIR);
  const finalPath = join(dir, SIDECAR_FILENAME);

  if (!manifest) {
    robustRemoveSync(finalPath);
    return undefined;
  }

  mkdirSync(dir, { recursive: true });
  const data = `${JSON.stringify(manifest, null, 2)}\n`;
  const tmpPath = `${finalPath}.${process.pid}.${randomBytes(6).toString('hex')}.tmp`;
  try {
    writeFileSync(tmpPath, data);
    robustRenameSync(tmpPath, finalPath);
  } catch (err) {
    // Best-effort cleanup: it must not replace the write error.
    try {
      rmSync(tmpPath, { force: true });
    } catch {
      // Keep the original error.
    }
    throw err;
  }
  return finalPath;
}
