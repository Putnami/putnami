import { file, gzipSync, spawn, write } from 'bun';
import { mkdirSync, readdirSync, rmSync } from 'node:fs';
import { delimiter as pathDelimiter } from 'node:path';
import { PutnamiConfig } from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { getProjectRoot, getWorkspaceRoot, joinPath } from '@putnami/utils';

export type CompilationResult = {
  route: string;
  path: string;
  isEntry: boolean;
};

export const buildEntrypoint = async (
  entrypoint: string,
  targetFile = 'hydrate.main.js',
  subdir = 'react',
): Promise<CompilationResult[]> => {
  const conf = useConfig(PutnamiConfig);
  const outdir = joinPath(getProjectRoot(), '.gen', conf.publicFolder, subdir);

  rmSync(outdir, { recursive: true, force: true });
  mkdirSync(outdir, { recursive: true });

  const bunPath = process.env['BUN_PATH'] || 'bun';

  const namePattern = targetFile.endsWith('.js') ? `${targetFile.slice(0, -3)}.[hash].js` : `${targetFile}.[hash].js`;

  const args = [
    bunPath,
    'build',
    '--production',
    '--target=browser',
    `--outdir=${outdir}`,
    '--format=esm',
    '--banner="use client"',
    '--splitting',
    '--sourcemap=none',
    `--entry-naming=${namePattern}`,
    '--chunk-naming=chunk.[hash].js',
    '--external=bun',
    '--conditions=browser',
    entrypoint,
  ];

  const workspaceRoot = getWorkspaceRoot();
  const projectRoot = getProjectRoot();
  const extensionRoot = joinPath(import.meta.dir, '..', '..', '..');
  const extensionNodeModules = joinPath(extensionRoot, 'node_modules');

  // Build NODE_PATH to help resolve packages from both workspace and project node_modules
  // This is needed in fresh workspaces and isolated linkers where dependencies may
  // only be present under the extension package itself.
  const nodePaths = Array.from(
    new Set([joinPath(workspaceRoot, 'node_modules'), joinPath(projectRoot, 'node_modules'), extensionNodeModules]),
  ).join(pathDelimiter);

  const proc = spawn(args, {
    // Use workspace root so Bun can resolve dependencies from workspace node_modules
    cwd: workspaceRoot,
    env: {
      ...process.env,
      NODE_PATH: nodePaths,
    },
    stderr: 'pipe',
    stdout: 'pipe',
  });

  // Add timeout to detect hanging builds
  const timeout = 60_000; // 60 seconds
  const timeoutPromise = new Promise<never>((_, reject) => {
    setTimeout(() => reject(new Error(`React build timed out after ${timeout}ms`)), timeout);
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
    throw new Error(`React build failed (exit code ${proc.exitCode}):\nstderr: ${err}\nstdout: ${out}`);
  }

  const generatedFiles = readdirSync(outdir)
    .filter((f) => f.endsWith('.js'))
    .sort();
  const targetBase = targetFile.replace(/\.js$/, '');

  const results = await Promise.all(
    generatedFiles.map(async (fileName) => {
      const filePath = joinPath(outdir, fileName);
      const content = await file(filePath).arrayBuffer();

      const isEntry = fileName.startsWith(`${targetBase}.`);

      const gzipped = gzipSync(content);
      const gzFileName = `${fileName}.gz`;
      await write(joinPath(outdir, gzFileName), gzipped);
      rmSync(filePath);

      return {
        route: `/${subdir}/${fileName}`,
        path: `${subdir}/${gzFileName}`,
        isEntry,
      };
    }),
  );

  return results;
};
