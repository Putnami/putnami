#!/usr/bin/env bun
/**
 * @putnami/client:putnami-client-generate — the TypeScript client emitter.
 *
 * Renders the TypeScript client of a provider's committed OpenAPI contract into
 * a client project, with no running app. The client project's
 * `client-generate` command runs it, as the client, from committed files only:
 * the provider's contract and the client's own `generatedFrom`
 * (tooling/clientgen-extension ADR 0007). The provider may be written in any
 * language.
 *
 * Usage:
 *   putnami-client-generate --contract <abs path to openapi.json> \
 *     --output <abs client project dir> --package-name <npm package name> \
 *     [--format-project <abs dir whose Biome and EditorConfig format the output>]
 *
 * It writes exactly `<output>/src/**`, replacing that tree, and no
 * package.json, tsconfig.json or manifest. A contract without a document-level
 * x-putnami-client fails. A first-party contract with no operation removes
 * `<output>/src` and exits 0. Any failure exits non-zero with its message on
 * stderr.
 */
import { GeneratedFileCanonicalizationError } from '../src/generator/generated-file-canonicalizer';
import { ClientGenerationError } from '../src/generator/openapi-reader';
import { generateClient } from '../src/generator/project-clients';

const USAGE =
  'usage: putnami-client-generate --contract <openapi.json> --output <client dir> --package-name <name> [--format-project <dir>]';

/** Thrown for a command line this emitter does not accept. */
class UsageError extends Error {}

try {
  const flags = parseFlags(process.argv.slice(2));
  generateClient({
    contractPath: required(flags, 'contract'),
    outputDir: required(flags, 'output'),
    packageName: required(flags, 'package-name'),
    ...(flags.has('format-project') ? { formatProjectRoot: required(flags, 'format-project') } : {}),
  });
} catch (error) {
  // The task that runs this emitter reports only its exit status and output,
  // so every failure says why. A named refusal prints its message; any other
  // exception prints its stack, which starts with the message.
  process.stderr.write(`${describeFailure(error)}\n`);
  process.exit(1);
}

function parseFlags(args: readonly string[]): Map<string, string> {
  const known = new Set(['contract', 'output', 'package-name', 'format-project']);
  const flags = new Map<string, string>();
  for (let i = 0; i < args.length; i++) {
    const name = args[i].startsWith('--') ? args[i].slice(2) : '';
    if (!known.has(name)) throw new UsageError(`unknown argument ${JSON.stringify(args[i])}\n${USAGE}`);
    if (flags.has(name)) throw new UsageError(`--${name} is given twice\n${USAGE}`);
    const value = args[++i];
    if (value === undefined) throw new UsageError(`--${name} needs a value\n${USAGE}`);
    flags.set(name, value);
  }
  return flags;
}

function required(flags: Map<string, string>, name: string): string {
  const value = flags.get(name);
  if (!value) throw new UsageError(`--${name} is required\n${USAGE}`);
  return value;
}

function describeFailure(error: unknown): string {
  if (
    error instanceof UsageError ||
    error instanceof GeneratedFileCanonicalizationError ||
    error instanceof ClientGenerationError
  ) {
    return error.message;
  }
  if (error instanceof Error) return error.stack ?? `${error.name}: ${error.message}`;
  return `putnami-client-generate failed: ${String(error)}`;
}
