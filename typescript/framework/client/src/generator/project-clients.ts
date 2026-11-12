import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, resolve, sep } from 'node:path';
import { fileExists } from '@putnami/utils';
import { canonicalizeGeneratedFiles } from './generated-file-canonicalizer';
import { readOpenApiSource } from './openapi-reader';
import { generateTypeScriptClient } from './ts/ts-generator';

/** The directory of a client project that holds its generated closure. */
const GENERATED_DIR = 'src';

/** What {@link generateClient} renders, and where. */
export interface GenerateClientOptions {
  /** Absolute path of the provider's committed OpenAPI contract. */
  contractPath: string;
  /** Absolute directory of the client project the closure is written into. */
  outputDir: string;
  /** npm package name of the client project, which its own `package.json` declares. */
  packageName: string;
  /**
   * Directory whose Biome configuration and EditorConfig format the output, at
   * the same relative paths. It defaults to `outputDir`; a caller that renders
   * into an isolated copy of the client project names the real one.
   */
  formatProjectRoot?: string;
}

/** Result of {@link generateClient}. */
export interface GenerateClientResult {
  /** False when the contract declares no operation, so no closure exists. */
  generated: boolean;
  /** Output-relative, forward-slash paths of the files written this run. */
  files: string[];
}

/**
 * Render the TypeScript client of a first-party contract into the client
 * project at `outputDir`, from that contract alone.
 *
 * The output is the generated closure only: every file under `src/`, which this
 * run replaces whole. The client project's authored files (its `putnami.json`,
 * `package.json` and `tsconfig.json`) sit beside it, and generation never reads,
 * writes or removes them. No manifest is written: Git holds no generated client
 * file (tooling/clientgen-extension ADR 0007).
 *
 * Each operation's design attribution comes from its
 * `x-putnami-client.producer`, which the provider wrote into its contract.
 *
 * A contract without a document-level `x-putnami-client` fails. A first-party
 * contract that declares no operation removes `src/` and returns
 * `generated: false`. Every file is rendered and formatted before any byte on
 * disk changes, so a refusal or a formatter failure leaves the output as it was.
 */
export function generateClient(options: GenerateClientOptions): GenerateClientResult {
  if (!options.packageName.trim()) {
    throw new Error('clientgen: the client package name is empty.');
  }
  if (!fileExists(options.contractPath)) {
    throw new Error(`clientgen: no contract at ${options.contractPath}.`);
  }
  const spec = readOpenApiSource(readFileSync(options.contractPath, 'utf8'), { mode: 'firstParty' });
  const outputDir = resolve(options.outputDir);
  const generatedDir = resolve(outputDir, GENERATED_DIR);
  if (spec.services.length === 0) {
    rmSync(generatedDir, { recursive: true, force: true });
    return { generated: false, files: [] };
  }

  const formatRoot = resolve(options.formatProjectRoot ?? outputDir);
  const files = canonicalizeGeneratedFiles(
    formatRoot,
    formatRoot,
    generateTypeScriptClient(spec, { packageName: options.packageName }),
  );
  for (const file of files) {
    const filePath = resolve(outputDir, file.path);
    if (!filePath.startsWith(`${generatedDir}${sep}`)) {
      throw new Error(`clientgen: generated file "${file.path}" is outside ${GENERATED_DIR}/.`);
    }
  }

  rmSync(generatedDir, { recursive: true, force: true });
  for (const file of files) {
    const filePath = resolve(outputDir, file.path);
    mkdirSync(dirname(filePath), { recursive: true });
    writeFileSync(filePath, file.content);
  }
  return { generated: true, files: files.map((file) => file.path) };
}
