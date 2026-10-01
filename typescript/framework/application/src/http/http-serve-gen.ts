import { mkdirSync, writeFileSync } from 'node:fs';
import {
  fileExists,
  getDirectoryName,
  getProjectRoot,
  joinPath,
  type PackageJson,
  readPackageJson,
  relativePath,
  resolvePackageExportPath,
  toPosixPath,
  updatePackageJson,
} from '@putnami/utils';

// ---------------------------------------------------------------------------
// Serve entrypoint generation — used by HttpPlugin.generate()
// ---------------------------------------------------------------------------

export function generateServeFiles(debug?: (msg: string) => void): { exports?: Record<string, string> } {
  const projectRoot = getProjectRoot();
  const packageJson = readPackageJson(joinPath(projectRoot, 'package.json'));

  debug?.(`Project root: ${projectRoot}`);
  debug?.(`Package.json exports: ${JSON.stringify(packageJson?.exports || 'none')}`);

  // Check if export exists
  if (hasServeExport(packageJson?.exports)) {
    debug?.('./serve export found in package.json');
    // Export exists, but check if the file actually exists
    const exportedPath = resolvePackageExportPath(packageJson?.exports, './serve');
    if (exportedPath) {
      const fullPath = joinPath(projectRoot, exportedPath);
      debug?.(`Serve export path: ${exportedPath}, checking if file exists at: ${fullPath}`);
      if (fileExists(fullPath)) {
        // File exists, nothing to do
        debug?.('Serve file exists, skipping generation');
        return {};
      }
      debug?.('Serve file does not exist, will generate');
      // File doesn't exist, need to generate it
      const appEntrypoint = resolveAppEntrypoint(projectRoot, packageJson?.main);
      if (appEntrypoint) {
        debug?.(`Found app entrypoint: ${appEntrypoint}, generating serve file at: ${exportedPath}`);
        // Generate at the path specified in the export
        const servePath = generateServeEntrypointAtPath(projectRoot, appEntrypoint, exportedPath);
        return { exports: { './serve': servePath } };
      }
      debug?.('No app entrypoint found');
    }
    // Export exists but couldn't resolve path or no app entrypoint, return empty
    debug?.('Could not resolve serve export path or no app entrypoint, returning empty');
    return {};
  }

  debug?.('No ./serve export in package.json, checking for src/serve.ts or generating');

  // Export doesn't exist, generate file and add export
  const servePath = resolveServeEntrypoint(projectRoot, packageJson?.main);
  if (!servePath) {
    debug?.('Could not resolve or generate serve entrypoint');
    return {};
  }

  debug?.(`Generated/resolved serve entrypoint: ${servePath}`);
  ensureServeExportInPackageJson(projectRoot, servePath);

  return { exports: { './serve': servePath } };
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

function hasServeExport(exportsField: PackageJson['exports'] | undefined): boolean {
  if (!exportsField || typeof exportsField === 'string') {
    return false;
  }
  return './serve' in exportsField || '/serve' in exportsField || 'serve' in exportsField;
}

function resolveServeEntrypoint(projectRoot: string, mainEntry?: string): string | undefined {
  const serveEntrypoint = 'src/serve.ts';
  if (fileExists(joinPath(projectRoot, serveEntrypoint))) {
    return joinPath(projectRoot, serveEntrypoint);
  }

  const appEntrypoint = resolveAppEntrypoint(projectRoot, mainEntry);
  if (!appEntrypoint) {
    return undefined;
  }

  return generateServeEntrypoint(projectRoot, appEntrypoint);
}

function resolveAppEntrypoint(projectRoot: string, mainEntry?: string): string | undefined {
  const candidateEntrypoints = [mainEntry, 'src/main.ts', 'src/app.ts', 'src/index.ts'].filter(Boolean) as string[];

  for (const entrypoint of candidateEntrypoints) {
    const normalizedEntrypoint = entrypoint.replace(/^\.\//, '');
    if (fileExists(joinPath(projectRoot, normalizedEntrypoint))) {
      return normalizedEntrypoint;
    }
  }

  return undefined;
}

function generateServeEntrypoint(projectRoot: string, appEntrypoint: string): string {
  return generateServeEntrypointAtPath(projectRoot, appEntrypoint, '.gen/src/serve.ts');
}

function generateServeEntrypointAtPath(projectRoot: string, appEntrypoint: string, targetPath: string): string {
  const fullTargetPath = joinPath(projectRoot, targetPath);
  const targetDir = getDirectoryName(fullTargetPath);
  mkdirSync(targetDir, { recursive: true });

  const importTarget = joinPath(projectRoot, appEntrypoint);
  const importPath = normalizeImportPath(relativePath(targetDir, importTarget));
  // Route through bootstrapServe (not a bare app().start()) so a fatal startup
  // failure is logged as a single structured line and exits non-zero, matching
  // the compiled bundled-serve entrypoint instead of Bun's multi-line printer.
  const contents = `import { bootstrapServe } from '@putnami/application';\nimport { app } from '${importPath}';\n\nawait bootstrapServe(app);\n`;

  writeFileSync(fullTargetPath, contents, 'utf-8');

  return fullTargetPath;
}

function normalizeImportPath(pathValue: string): string {
  let normalized = pathValue.replace(/\\/g, '/').replace(/\.(ts|tsx|js|jsx)$/, '');
  if (!normalized.startsWith('.')) {
    normalized = `./${normalized}`;
  }
  return normalized;
}

function ensureServeExportInPackageJson(projectRoot: string, servePath: string): void {
  const packageJsonPath = joinPath(projectRoot, 'package.json');
  const packageJson = readPackageJson(packageJsonPath);
  if (!packageJson || hasServeExport(packageJson.exports)) {
    return;
  }

  // package.json export targets use forward slashes on every platform.
  const relativeServePath = toPosixPath(relativePath(projectRoot, servePath)).replace(/^\.\//, '');
  let exportsField: Exclude<PackageJson['exports'], string> = {};

  if (packageJson.exports && typeof packageJson.exports === 'object') {
    exportsField = { ...packageJson.exports };
  } else if (typeof packageJson.exports === 'string') {
    exportsField = { '.': packageJson.exports };
  }

  exportsField['./serve'] = { default: relativeServePath };

  updatePackageJson(packageJsonPath, { exports: exportsField });
}
