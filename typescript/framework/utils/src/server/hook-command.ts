/**
 * Hook command utilities for subprocess-based hooks.
 *
 * Provides a simplified command runner specifically designed for hooks:
 * - Simple argv parsing (--putnami-context, --help, plus custom flags)
 * - Automatic JSONL output and proper exit codes
 * - Help printed to stderr (stdout reserved for JSONL)
 */

import {
  createEvent,
  emitError,
  emitEvent,
  emitSummary,
  type HookContext,
  type HookEventMeta,
  HookExitCodes,
  validateHookContext,
} from './hook-events';
import { fileExists, readFileContent } from './index';

/**
 * Flag definition for hook commands.
 */
export interface HookFlagDefinition {
  flag: string;
  description?: string;
  type?: 'string' | 'boolean';
  required?: boolean;
  shorts?: string[];
}

/**
 * Hook model descriptor for defining hook CLI commands.
 */
export interface HookModelDescriptor<T extends object = StandardHookOptions> {
  name?: string;
  description?: string;
  flags: Record<string, HookFlagDefinition>;
  /** Create an extended model with additional flags */
  extend: <D extends { name?: string; description?: string; flags?: Record<string, HookFlagDefinition> }>(
    def: D,
  ) => HookModelDescriptor<T & Record<string, unknown>>;
}

function createHookModel<T extends object = StandardHookOptions>(opts: {
  name?: string;
  description?: string;
  flags: Record<string, HookFlagDefinition>;
}): HookModelDescriptor<T> {
  return {
    name: opts.name,
    description: opts.description,
    flags: opts.flags,
    extend<D extends { name?: string; description?: string; flags?: Record<string, HookFlagDefinition> }>(def: D) {
      return createHookModel<T & Record<string, unknown>>({
        name: def.name ?? opts.name,
        description: def.description ?? opts.description,
        flags: { ...opts.flags, ...(def.flags ?? {}) },
      });
    },
  };
}

/**
 * Standard hook model with common flags for all hook commands.
 */
export const standardHookModel = createHookModel<StandardHookOptions>({
  flags: {
    putnamiContext: {
      flag: '--putnami-context',
      description: 'Path to the context JSON file',
      type: 'string',
      required: true,
    },
    help: {
      flag: '--help',
      shorts: ['-h'],
      description: 'Show help',
      type: 'boolean',
    },
  },
});

/**
 * Options for running a hook command.
 */
export interface HookCommandOptions<T extends object> {
  /** The hook model descriptor */
  model: HookModelDescriptor<T>;
  /** The hook runner function */
  run: (options: T, context: HookContext) => Promise<HookResult> | HookResult;
  /** Extension name for meta events */
  extension: string;
  /** Hook name (e.g., 'preBuild') */
  hook: string;
  /** Version string for meta events */
  version?: string;
}

/**
 * Result returned by hook runner functions.
 */
export interface HookResult {
  /** Exports produced by the hook (e.g., { 'ssr-loader': '/path/to/loader.ts' }) */
  exports?: Record<string, string>;
  /** Assets produced by the hook (e.g., { 'public/bundle.js': '/absolute/path' }) */
  assets?: Record<string, string>;
  /** Custom summary data to include in the summary event */
  data?: Record<string, unknown>;
}

/**
 * Standard hook command options that all hook commands should support.
 */
export interface StandardHookOptions {
  /** Path to the context JSON file */
  putnamiContext: string;
  /** Show help */
  help?: boolean;
}

/**
 * Read and parse the hook context from a file path.
 */
export function readHookContext(contextPath: string): HookContext {
  if (!fileExists(contextPath)) {
    throw new Error(`Context file not found: ${contextPath}`);
  }
  const content = readFileContent(contextPath, 'utf-8');

  let parsed: unknown;
  try {
    parsed = JSON.parse(content);
  } catch (error) {
    throw new Error(`Invalid JSON in context file ${contextPath}: ${error instanceof Error ? error.message : error}`);
  }

  // The context file is untrusted input whose values flow into process.env and
  // build control flow, so validate the envelope before casting.
  const errors = validateHookContext(parsed);
  if (errors.length > 0) {
    const details = errors.map((e) => `${e.field}: ${e.message}`).join('; ');
    throw new Error(`Invalid hook context file ${contextPath}: ${details}`);
  }

  return parsed as HookContext;
}

/**
 * Parse command-line arguments using a hook model.
 */
interface HookFlagEntry {
  propertyName: string;
  def: HookFlagDefinition;
}

function buildFlagLookup<T extends object>(model: HookModelDescriptor<T>): Map<string, HookFlagEntry> {
  const flagMap = new Map<string, HookFlagEntry>();

  for (const [propertyName, def] of Object.entries(model.flags)) {
    const entry: HookFlagEntry = { propertyName, def };
    flagMap.set(def.flag, entry);
    for (const short of def.shorts ?? []) {
      flagMap.set(short, entry);
    }
  }

  return flagMap;
}

function splitFlagToken(arg: string): { flagName: string; inlineValue: string | undefined } {
  const eqIdx = arg.indexOf('=');
  if (eqIdx === -1) {
    return { flagName: arg, inlineValue: undefined };
  }

  return {
    flagName: arg.substring(0, eqIdx),
    inlineValue: arg.substring(eqIdx + 1),
  };
}

function parseFlagValue(
  entry: HookFlagEntry,
  inlineValue: string | undefined,
  args: string[],
  index: number,
): { value: unknown; nextIndex: number } {
  if (entry.def.type === 'string') {
    if (inlineValue !== undefined) {
      return { value: inlineValue, nextIndex: index };
    }
    return { value: args[index + 1], nextIndex: index + 1 };
  }

  if (inlineValue === undefined) {
    return { value: true, nextIndex: index };
  }

  return { value: inlineValue !== 'false', nextIndex: index };
}

function parseHookArgs<T extends object>(model: HookModelDescriptor<T>, args: string[]): T {
  const result: Record<string, unknown> = {};
  const flagMap = buildFlagLookup(model);

  for (let i = 0; i < args.length; i++) {
    const { flagName, inlineValue } = splitFlagToken(args[i]);
    const entry = flagMap.get(flagName);

    if (!entry) {
      continue;
    }

    const { value, nextIndex } = parseFlagValue(entry, inlineValue, args, i);
    result[entry.propertyName] = value;
    i = nextIndex;
  }

  return result as T;
}

/**
 * Print command help to stderr.
 * Hooks print help to stderr since stdout is reserved for JSONL.
 */
function printHelp(model: HookModelDescriptor, extension: string): void {
  const name = model.name || extension;
  const description = model.description || 'Hook command';

  let help = `\n${name} - ${description}\n\nUsage:\n  bunx ${extension} --putnami-context <path> [options]\n\nOptions:\n`;

  for (const [, def] of Object.entries(model.flags)) {
    const short = def.shorts?.[0] ? `, ${def.shorts[0]}` : '';
    const required = def.required ? ' (required)' : '';
    help += `  ${def.flag}${short}${required}\n`;
    if (def.description) {
      help += `      ${def.description}\n`;
    }
  }

  help += '\nContext File:\n';
  help += '  The context file should contain a HookContext JSON object with:\n';
  help += '  - workspaceRoot, projectRoot, extensionRoot, outputRoot, cacheRoot\n';
  help += '  - debug, hook, extension, config (optional)\n';

  process.stderr.write(help);
}

/**
 * Run a hook command.
 *
 * This is the main entry point for hook commands. It:
 * 1. Parses command-line arguments
 * 2. Emits a meta event
 * 3. Reads the context file
 * 4. Runs the hook function
 * 5. Emits a summary event
 * 6. Exits with appropriate code
 *
 * @example
 * ```ts
 * const generateModel = standardHookModel.extend({
 *   name: 'generate',
 *   description: 'Generate React bundles',
 * });
 *
 * runHookCommand({
 *   model: generateModel,
 *   extension: '@putnami/web',
 *   hook: 'preBuild',
 *   version: '0.0.3',
 *   run: async (options, context) => {
 *     // ... generate code ...
 *     return { exports: { 'ssr-loader': loaderPath } };
 *   },
 * });
 * ```
 */
export async function runHookCommand<T extends object>(options: HookCommandOptions<T>): Promise<never> {
  const startTime = Date.now();
  const { model, run, extension, hook, version } = options;

  const args = process.argv.slice(2);

  // Handle --help early
  if (args.includes('--help') || args.includes('-h')) {
    printHelp(model, extension);
    process.exit(HookExitCodes.SUCCESS);
  }

  // Parse arguments
  let parsed: T;
  try {
    parsed = parseHookArgs(model, args);
  } catch (e) {
    process.stderr.write(`Error parsing arguments: ${e instanceof Error ? e.message : String(e)}\n`);
    printHelp(model, extension);
    process.exit(HookExitCodes.INVALID_CONFIG);
  }

  const parsedRecord = parsed as Record<string, unknown>;

  // Validate required context path
  if (!parsedRecord['putnamiContext']) {
    process.stderr.write('Error: --putnami-context is required\n');
    printHelp(model, extension);
    process.exit(HookExitCodes.INVALID_CONFIG);
  }

  // Emit meta event
  emitEvent(
    createEvent<HookEventMeta>({
      type: 'meta',
      level: 'info',
      message: `Starting ${hook}`,
      data: {
        extension,
        hook,
        version: version || '0.0.0',
      },
    }),
  );

  try {
    // Read context
    const context = readHookContext(parsedRecord['putnamiContext'] as string);

    // Set environment
    process.env['PUTNAMI_PROJECT_ROOT'] = context.projectRoot;
    process.env['PUTNAMI_WORKSPACE_ROOT'] = context.workspaceRoot;
    process.env['PUTNAMI_PREBUILD_CONTEXT'] = 'true';

    // Run the hook
    const result = await run(parsed, context);

    // Emit summary. The summary line can exceed the 64KB pipe buffer, and
    // process.exit() discards queued stdout writes — await actual delivery
    // before exiting or the consumer receives a truncated, unparsable event.
    const durationMs = Date.now() - startTime;
    await emitSummary(`${hook} completed`, {
      durationMs,
      outputs: Object.keys(result.exports || {}).length + Object.keys(result.assets || {}).length,
      exports: result.exports,
      assets: result.assets,
      ...result.data,
    });

    process.exit(HookExitCodes.SUCCESS);
  } catch (error) {
    await emitError(
      error instanceof Error ? error.message : String(error),
      error instanceof Error ? error : undefined,
      false,
    );
    process.exit(HookExitCodes.BUILD_ERROR);
  }
}
