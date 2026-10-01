import { file, gzipSync, spawn, write } from 'bun';
import { mkdirSync, readdirSync, rmSync } from 'node:fs';
import { delimiter as pathDelimiter } from 'node:path';
import { getWorkspaceRoot, joinPath } from '@putnami/utils';

/** One emitted browser artifact. */
export type CompilationResult = {
  /** Public route the artifact is served at. */
  route: string;
  /** Path of the gzipped file, relative to the public folder. */
  path: string;
  /** Whether the artifact is the bundle entry. */
  isEntry: boolean;
};

const TARGET_FILE = 'analytics.js';
const SUBDIR = 'analytics';
const BUILD_TIMEOUT_MS = 60_000;

/**
 * Bundles the browser tracker into `<projectRoot>/.gen/<publicFolder>/analytics/`.
 *
 * Copied from the (unexported) `buildEntrypoint` of `@putnami/web`, without
 * `--splitting` (the tracker is a single file with no shared chunks) and
 * without the `"use client"` banner. The output directory is deleted first, so
 * the subdirectory must stay `analytics` and never `react` or `react-islands`.
 *
 * @param entrypoint - Absolute path of the tracker entry module.
 * @param publicFolder - The `PutnamiConfig.publicFolder` value.
 * @param projectRoot - Absolute path of the consuming project.
 * @returns The gzipped artifacts produced by the bundle.
 */
export const buildTrackerBundle = async (
  entrypoint: string,
  publicFolder: string,
  projectRoot: string,
): Promise<CompilationResult[]> => {
  const outdir = joinPath(projectRoot, '.gen', publicFolder, SUBDIR);

  rmSync(outdir, { recursive: true, force: true });
  mkdirSync(outdir, { recursive: true });

  const bunPath = process.env['BUN_PATH'] || 'bun';
  const namePattern = `${TARGET_FILE.slice(0, -3)}.[hash].js`;

  const args = [
    bunPath,
    'build',
    '--production',
    '--target=browser',
    `--outdir=${outdir}`,
    '--format=esm',
    '--sourcemap=none',
    `--entry-naming=${namePattern}`,
    '--external=bun',
    '--conditions=browser',
    entrypoint,
  ];

  const workspaceRoot = getWorkspaceRoot();
  const extensionRoot = joinPath(import.meta.dir, '..');
  const extensionNodeModules = joinPath(extensionRoot, 'node_modules');

  // Resolve packages from the workspace, the consuming project, and the
  // extension itself — isolated linkers place dependencies in any of the three.
  const nodePaths = Array.from(
    new Set([joinPath(workspaceRoot, 'node_modules'), joinPath(projectRoot, 'node_modules'), extensionNodeModules]),
  ).join(pathDelimiter);

  const proc = spawn(args, {
    cwd: workspaceRoot,
    env: {
      ...process.env,
      NODE_PATH: nodePaths,
    },
    stderr: 'pipe',
    stdout: 'pipe',
  });

  const timeoutPromise = new Promise<never>((_, reject) => {
    setTimeout(
      () => reject(new Error(`Analytics tracker build timed out after ${BUILD_TIMEOUT_MS}ms`)),
      BUILD_TIMEOUT_MS,
    );
  });

  try {
    await Promise.race([proc.exited, timeoutPromise]);
  } catch (error) {
    proc.kill();
    throw error;
  }

  if (proc.exitCode !== 0) {
    const err = await new Response(proc.stderr).text();
    const out = await new Response(proc.stdout).text();
    throw new Error(`Analytics tracker build failed (exit code ${proc.exitCode}):\nstderr: ${err}\nstdout: ${out}`);
  }

  const generatedFiles = readdirSync(outdir)
    .filter((f) => f.endsWith('.js'))
    .sort();
  const targetBase = TARGET_FILE.replace(/\.js$/, '');

  return await Promise.all(
    generatedFiles.map(async (fileName) => {
      const filePath = joinPath(outdir, fileName);
      const content = await file(filePath).arrayBuffer();

      const gzipped = gzipSync(content);
      const gzFileName = `${fileName}.gz`;
      await write(joinPath(outdir, gzFileName), gzipped);
      rmSync(filePath);

      return {
        route: `/${SUBDIR}/${fileName}`,
        path: `${SUBDIR}/${gzFileName}`,
        isEntry: fileName.startsWith(`${targetBase}.`),
      };
    }),
  );
};
