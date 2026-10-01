import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { fileExists, getDirectoryName, getWorkspaceRoot, joinPath } from '@putnami/utils';
import type { GenerateResult, Module, Plugin } from '../application';
import { emitTypeScript } from './contract-emit';
import type { ContractManifest } from './contract-ir.types';

/** One canonical-IR → TypeScript-twin emission, resolved against the workspace root. */
export interface ContractTwinSpec {
  /** Canonical contract IR (`contracts.json`), workspace-root-relative. */
  manifest: string;
  /** TypeScript twin (`contracts.gen.ts`) to write, workspace-root-relative. */
  output: string;
}

/**
 * The identity contract twin. `protocols/identity` is authored as a contract
 * manifest, so — like its Go twin (`contracts.gen.go`) — the TypeScript twin is
 * a pure function of the canonical IR. Keeping the emitter pure is what makes
 * `putnami contracts generate` hermetic; this spec declares the on-disk write
 * that consumes its output.
 */
export const IDENTITY_CONTRACT_TWIN: ContractTwinSpec = {
  manifest: 'protocols/identity/schema/contracts.json',
  output: 'protocols/identity/schema/contracts.gen.ts',
};

/**
 * Generate-phase plugin that writes on-disk TypeScript contract twins from
 * canonical contract IR.
 *
 * It only wires bytes to disk: each twin is produced by the golden-pinned pure
 * emitter {@link emitTypeScript}, never reimplemented here, so a committed twin
 * stays byte-identical to a fresh generation (the drift test enforces this). A
 * spec whose manifest is absent is skipped rather than failing the build, so an
 * app that adopts the plugin without a given contract still builds.
 */
export class ContractTwinPlugin implements Plugin {
  constructor(private readonly specs: readonly ContractTwinSpec[]) {}

  generate(_owner: Module): GenerateResult {
    const workspaceRoot = getWorkspaceRoot();
    for (const spec of this.specs) {
      const manifestPath = joinPath(workspaceRoot, spec.manifest);
      if (!fileExists(manifestPath)) continue;
      const manifest = JSON.parse(readFileSync(manifestPath, 'utf8')) as ContractManifest;
      const outputPath = joinPath(workspaceRoot, spec.output);
      mkdirSync(getDirectoryName(outputPath), { recursive: true });
      writeFileSync(outputPath, emitTypeScript(manifest));
    }
    return {};
  }
}

/**
 * Create a {@link ContractTwinPlugin}. Defaults to the identity contract twin.
 */
export function contractTwins(specs: readonly ContractTwinSpec[] = [IDENTITY_CONTRACT_TWIN]): ContractTwinPlugin {
  return new ContractTwinPlugin(specs);
}
