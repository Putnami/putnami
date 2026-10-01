import { join } from 'node:path';
import { fileExists, readFileContent } from './index';

/**
 * Project-level configuration in `putnami.json`.
 */
export interface ProjectConfig {
  name?: string;
  description?: string;
  main?: string;
  bin?: Record<string, string> | string;
  exports?: string | Record<string, string | { types?: string; import?: string; require?: string; default?: string }>;
  tags?: string[];
  type?: string;
  publish?: string[];
  runsWith?: string[];
  build?: {
    assets?: { from?: string }[];
    compile?: Record<string, string>;
  };
  dependencies?: string[];
  extensions?: string[];
  options?: Record<string, Record<string, unknown>>;
  /**
   * Accepted but ignored. Project-level job definitions are not implemented:
   * the CLI parses this key, warns that it declared one, and never plans it.
   * Kept opaque on purpose so nothing here can be mistaken for a wired shape —
   * use `disable.jobs` to opt out of an extension-provided job, or the
   * project's `tasks` block for per-project execution tuning.
   */
  jobs?: Record<string, unknown>;
  disable?: {
    jobs?: string[];
  };
}

const CONFIG_FILENAME = 'putnami.json';
const LEGACY_CONFIG_FILENAME = '.putnamirc.json';

/**
 * Read a project-level config file from a directory.
 * Prefers `putnami.json`, falls back to `.putnamirc.json`.
 */
export function readProjectConfigFile(absoluteDirPath: string): ProjectConfig | undefined {
  let filePath = join(absoluteDirPath, CONFIG_FILENAME);
  if (!fileExists(filePath)) {
    filePath = join(absoluteDirPath, LEGACY_CONFIG_FILENAME);
    if (!fileExists(filePath)) {
      return undefined;
    }
  }
  try {
    const content = readFileContent(filePath, 'utf8');
    return JSON.parse(content) as ProjectConfig;
  } catch {
    return undefined;
  }
}
