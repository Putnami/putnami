import { useConfig } from '@putnami/runtime';
import { GeneratorHelper, joinPath, relativePath, getProjectRoot, toPosixPath } from '@putnami/utils';
import { type IslandStrategy, isIslandComponent } from '../../client/island/island-types';
import { PutnamiReactConfig } from '../react-ssr.config';
import { buildEntrypoint, type CompilationResult } from './build';

/**
 * Load a `*.island.tsx` module at build time and return its hydration strategy.
 * Defaults to `load` when the module can't be introspected.
 */
export async function detectIslandStrategy(absFilePath: string): Promise<IslandStrategy> {
  try {
    const mod = (await import(absFilePath)) as Record<string, unknown>;
    const def = mod['default'];
    if (isIslandComponent(def)) {
      return def.__island.strategy;
    }
  } catch {
    // Unresolvable / throwing module → assume the default strategy.
  }
  return 'load';
}

interface IslandEntry {
  file: string;
  id: string;
  relativeGenDir: string;
}

/**
 * Generates the client-side islands entry: a single module that scans the page
 * for island markers and lazily hydrates each present island. Each island is a
 * dynamic import, so unused islands never reach the browser.
 */
export class IslandClientGenerator {
  private islands: IslandEntry[] = [];

  constructor(
    private islandsEntryPath: string,
    private relativeGenDir = 'src/app',
  ) {}

  add(file: string, id: string, relativeGenDir?: string): void {
    this.islands.push({ file, id, relativeGenDir: relativeGenDir ?? this.relativeGenDir });
  }

  get count(): number {
    return this.islands.length;
  }

  write(): void {
    const generator = new GeneratorHelper(this.islandsEntryPath);
    generator
      .appendHead(
        `// generated islands entry exploring '${toPosixPath(relativePath(getProjectRoot(), this.relativeGenDir))}'`,
      )
      .appendHead(`import { runIslands } from '@putnami/web';`);

    generator.append('runIslands({');
    for (const island of this.islands) {
      const lazyImport = generator.getLazyImport(joinPath(island.relativeGenDir, island.file));
      generator.append(`  ${JSON.stringify(island.id)}: ${lazyImport},`);
    }
    generator.append('});');

    const conf = useConfig(PutnamiReactConfig);
    if (!conf.buildEnable) {
      return;
    }
    generator.write();
  }

  build(): Promise<CompilationResult[]> {
    return buildEntrypoint(this.islandsEntryPath, 'islands.js', 'react-islands');
  }
}
