import { existsSync } from 'node:fs';
import { basename, dirname, isAbsolute, join, relative } from 'node:path';
import type { InferConfig } from '@putnami/runtime';
import type { PutnamiReactConfig } from './react-ssr.config';
import { normalizeRoutePrefix } from './react-ssr.utils';

interface ResolvedScanRoot {
  scanPath: string;
  relativePath: string;
  routePrefix?: string;
}

export function inferRoutePrefix(scanPath: string): string | undefined {
  const scanDirName = basename(scanPath);
  if (scanDirName === 'web' || scanDirName === '(web)') {
    const parent = basename(dirname(scanPath));
    return parent ? `/${parent}` : undefined;
  }
  return undefined;
}

export function resolveScanRoots(
  projectRoot: string,
  config: InferConfig<typeof PutnamiReactConfig>,
): ResolvedScanRoot[] {
  const roots: ResolvedScanRoot[] = [];

  if (config.scanRoots && config.scanRoots.length > 0) {
    for (const root of config.scanRoots) {
      const scanPath = isAbsolute(root.path) ? root.path : join(projectRoot, root.path);
      if (!existsSync(scanPath)) {
        continue;
      }

      roots.push({
        scanPath,
        relativePath: relative(projectRoot, scanPath),
        routePrefix: normalizeRoutePrefix(root.routePrefix) ?? inferRoutePrefix(scanPath),
      });
    }
  }

  if (roots.length > 0) {
    return roots;
  }

  const defaultScanPath = join(projectRoot, 'src', 'app');
  const scanPath = config.scanPath
    ? isAbsolute(config.scanPath)
      ? config.scanPath
      : join(projectRoot, config.scanPath)
    : existsSync(defaultScanPath)
      ? defaultScanPath
      : undefined;

  if (!scanPath || !existsSync(scanPath)) {
    return [];
  }

  return [
    {
      scanPath,
      relativePath: relative(projectRoot, scanPath),
      routePrefix: undefined,
    },
  ];
}
