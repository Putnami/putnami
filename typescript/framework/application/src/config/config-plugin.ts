import { mkdirSync, writeFileSync } from 'node:fs';
import { error, resetConfigLoader, useLogger } from '@putnami/runtime';
import {
  fileExists,
  getCurrentProject,
  getProject,
  joinPath,
  listProjectDependencies,
  mergeDeep,
  readFileContent,
} from '@putnami/utils';
import type { GenerateResult, Module, Plugin } from '../application';

/**
 * Config Plugin
 * Discovers and merges config files from dependent modules during generate phase
 */
export class ConfigPlugin implements Plugin {
  private generatedConfigPath: string | undefined;

  constructor(private env: string) {}

  /**
   * Generate phase: discover and merge configs from dependencies
   */
  async generate(_: Module): Promise<GenerateResult> {
    if (process.env.CONFIG_DATA || process.env.K_SERVICE) {
      return {};
    }
    const project = getCurrentProject() || error('No project found');

    const dependencies = listProjectDependencies(project.name);

    const projectToScan = [...dependencies, project.name];
    let mergedConfig: Record<string, unknown> = {};
    for (const dep of projectToScan) {
      const depConfig = this.loadDependencyConfig(dep);
      if (depConfig) {
        mergedConfig = mergeDeep(mergedConfig, depConfig);
      }
    }
    // Write merged config to .gen folder
    if (Object.keys(mergedConfig).length > 0) {
      const projectPath = getProject(project.name)?.path;
      if (!projectPath) {
        return {};
      }
      const outputDir = joinPath(projectPath, '.gen/conf');

      try {
        mkdirSync(outputDir, { recursive: true });

        this.generatedConfigPath = joinPath(outputDir, `.env.${this.env}.yaml`);
        writeFileSync(this.generatedConfigPath, Bun.YAML.stringify(mergedConfig, null, 2));

        process.env.CONFIG_DATA = JSON.stringify(mergedConfig);
        resetConfigLoader();

        // Return assets so the config file gets copied to dist
        const destPath = joinPath('conf', `.env.${this.env}.yaml`);
        return {
          assets: {
            [destPath]: this.generatedConfigPath,
          },
          exports: {
            'config-loader': this.generatedConfigPath,
          },
        };
      } catch (cause) {
        throw new Error(`Failed to write generated config to ${outputDir}`, { cause });
      }
    }

    return {};
  }

  /**
   * Load config from a dependency's config folder
   */
  private loadDependencyConfig(depName: string): Record<string, unknown> | undefined {
    const project = getProject(depName);
    if (!project) {
      return undefined;
    }

    const configPath = joinPath(project.path, 'conf', `.env.${this.env}.yaml`);
    if (fileExists(configPath)) {
      try {
        const content = readFileContent(configPath, 'utf-8');
        return (Bun.YAML.parse(content) as Record<string, unknown>) || {};
      } catch (cause) {
        // The file exists (fileExists gate above) but failed to read/parse —
        // surface it instead of silently dropping the dependency's config,
        // which otherwise shows up as a confusing "missing config" downstream.
        useLogger('putnami').warn(`Failed to parse config file ${configPath}`, {
          configPath,
          dependency: depName,
          error: cause instanceof Error ? cause.message : String(cause),
        });
        return undefined;
      }
    }

    return undefined;
  }
}

/**
 * Create a config plugin
 * @param options.env Environment name (default: 'local' or 'test' based on NODE_ENV)
 */
export const config = (options?: { env?: string }) => {
  const isTest = process.env.NODE_ENV === 'test';
  const env = options?.env || (isTest ? 'test' : 'local');

  return new ConfigPlugin(env);
};
