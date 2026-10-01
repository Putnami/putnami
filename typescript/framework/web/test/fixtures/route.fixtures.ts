import type { ReactPluginConfig } from '../../src/ssr/react-plugin';
import type { PutnamiReactConfigExtras } from '../../src/ssr/react-ssr.config';

export const createMockReactConfig = (
  overrides: Partial<ReactPluginConfig & PutnamiReactConfigExtras> = {},
): ReactPluginConfig & PutnamiReactConfigExtras => ({
  port: 3000,
  publicFolder: 'public',
  skipLoading: false,
  scanFolder: 'app',
  autoScan: true,
  buildEnable: true,
  sourcemap: 'external',
  minify: true,
  splitting: false,
  ssrTimeout: 3000,
  scriptsFolder: 'scripts',
  isDevelopment: true,
  ...overrides,
});
