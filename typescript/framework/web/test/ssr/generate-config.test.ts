import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { resolveGenerationConfig } from '../../bin/generate-config';

describe('web generation configuration', () => {
  specTest(
    'rejects a React-only project option before generation starts',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'generation-configuration',
      check: 'misplaced-project-react-config-fails-before-generation',
    },
    () => {
      expect(() => resolveGenerationConfig(undefined, { scanRoots: [{ path: 'src/app' }] })).toThrow(
        'options["@putnami/web:generate"].scanRoots is React-only; use options["@putnami/web:generate"].react.scanRoots',
      );
    },
  );

  specTest(
    'gives nested project React settings precedence over package settings',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'generation-configuration',
      check: 'nested-project-react-config-takes-precedence',
    },
    () => {
      const resolved = resolveGenerationConfig(
        {
          publicFolder: 'legacy-public',
          scanPath: 'legacy-flat',
          react: { scanPath: 'legacy-nested', minify: false },
        },
        {
          publicFolder: 'project-public',
          react: { scanPath: 'project-nested', splitting: true },
        },
      );

      expect(resolved.shared).toEqual({ publicFolder: 'project-public' });
      expect(resolved.react).toEqual({
        scanPath: 'project-nested',
        minify: false,
        splitting: true,
      });
    },
  );

  specTest(
    'keeps flat package React settings compatible',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'generation-configuration',
      check: 'flat-package-react-config-remains-compatible',
    },
    () => {
      const scanRoots = [{ path: 'src/projects/web', routePrefix: '/projects' }];
      const resolved = resolveGenerationConfig({ publicFolder: 'assets', scanRoots }, undefined);

      expect(resolved.shared).toEqual({ publicFolder: 'assets' });
      expect(resolved.react).toEqual({ scanRoots });
    },
  );
});
