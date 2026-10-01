#!/usr/bin/env bun
/**
 * @putnami/client:putnami-client-generate — standalone TypeScript REST client emitter.
 *
 * Emits the TypeScript client for a provider from its generated OpenAPI spec,
 * with no running app. The workspace `clientgen` command runs it (in its own Bun
 * toolchain) to produce a TypeScript client for a provider written in ANOTHER
 * language — e.g. a Go service — reading that provider's OWN .gen spec. So
 * cross-language client emission is mediated through the shared spec artifact and
 * never shells one extension out to another.
 *
 * Usage:
 *   putnami-client-generate [--project DIR] [--format-project DIR]
 *
 * --project defaults to the current directory (the workspace runner sets the task
 * cwd to the project root). The project must already have been built (putnami
 * build) so its .gen/clientgen/config.json and OpenAPI spec exist.
 *
 * --format-project names the ORIGINAL provider project root when --project is an
 * isolated mirror of it. Canonicalization then resolves the Biome package,
 * configuration, overrides and EditorConfig against the provider's real identity
 * instead of the mirror's, so a mirrored render produces the provider's bytes.
 * It defaults to --project.
 */
import { generateProjectClients } from '../src/generator/project-clients';
import { GeneratedFileCanonicalizationError } from '../src/generator/generated-file-canonicalizer';
import { ClientGenerationError } from '../src/generator/openapi-reader';

const args = process.argv.slice(2);
let project = '';
let formatProject = '';
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--project') {
    project = args[++i] ?? '';
  } else if (args[i] === '--format-project') {
    formatProject = args[++i] ?? '';
  } else if (args[i] === '--putnamiContext') {
    // Accepted and ignored: the task sets cwd to the project root, so --project
    // (or the cwd default below) is authoritative.
    i++;
  }
}

try {
  const _res = generateProjectClients(
    project || process.cwd(),
    formatProject ? { formatProjectRoot: formatProject } : {},
  );
} catch (error) {
  // Every failure must say why: the workspace task that runs this shim reports
  // only its exit status and output. A named refusal prints its message; any
  // other exception prints its stack, which already starts with the message.
  process.stderr.write(`${describeFailure(error)}\n`);
  process.exit(1);
}

function describeFailure(error: unknown): string {
  if (error instanceof GeneratedFileCanonicalizationError || error instanceof ClientGenerationError) {
    return error.message;
  }
  if (error instanceof Error) return error.stack ?? `${error.name}: ${error.message}`;
  return `putnami-client-generate failed: ${String(error)}`;
}
