import { PutnamiConfig } from '@putnami/application';
import type { InferConfig } from '@putnami/runtime';
import { PutnamiReactConfig } from '../src/ssr/react-ssr.config';

type SharedGenerationConfig = Partial<InferConfig<typeof PutnamiConfig>>;
type ReactGenerationConfig = Partial<InferConfig<typeof PutnamiReactConfig>>;

interface ResolvedGenerationConfig {
  shared: SharedGenerationConfig;
  react: ReactGenerationConfig;
}

const sharedKeys = new Set(Object.keys(PutnamiConfig.schema));
const reactOnlyKeys = Object.keys(PutnamiReactConfig.schema)
  .filter((key) => !sharedKeys.has(key))
  .sort();

function asBlock(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}

function selectKeys(block: Record<string, unknown>, keys: Iterable<string>): Record<string, unknown> {
  const selected: Record<string, unknown> = {};
  for (const key of keys) {
    if (Object.hasOwn(block, key)) {
      selected[key] = block[key];
    }
  }
  return selected;
}

/**
 * Resolves the build-hook configuration without letting one schema's keys leak
 * into the other. Project putnami.json uses the explicit nested React shape;
 * package.json#putnami retains its historical flat React compatibility.
 */
export function resolveGenerationConfig(
  legacyPackageBlock: unknown,
  configuredProjectBlock: unknown,
): ResolvedGenerationConfig {
  const legacy = asBlock(legacyPackageBlock);
  const configured = asBlock(configuredProjectBlock);

  const misplacedKey = reactOnlyKeys.find((key) => Object.hasOwn(configured, key));
  if (misplacedKey) {
    const currentPath = `options["@putnami/web:generate"].${misplacedKey}`;
    const correctedPath = `options["@putnami/web:generate"].react.${misplacedKey}`;
    throw new Error(
      `Invalid @putnami/web generation configuration: ${currentPath} is React-only; use ${correctedPath}`,
    );
  }

  const { react: legacyReact } = legacy;
  const { react: configuredReact } = configured;

  return {
    shared: {
      ...selectKeys(legacy, sharedKeys),
      ...selectKeys(configured, sharedKeys),
    } as SharedGenerationConfig,
    react: {
      ...selectKeys(legacy, reactOnlyKeys),
      ...selectKeys(asBlock(legacyReact), reactOnlyKeys),
      ...selectKeys(asBlock(configuredReact), reactOnlyKeys),
    } as ReactGenerationConfig,
  };
}
