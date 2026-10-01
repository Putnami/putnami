import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  CLIENTGEN_DEFAULTS,
  type ClientGenConfig,
  resolveClientGenConfig,
  serializeClientGenConfig,
} from '../../src/generator/config.type';

const ctx = { defaultTsPackageName: '@demo/widgets-client' };

/**
 * The shared byte-parity fixture. The Go describer's writer
 * (`writeClientGenConfig`, asserted in go/framework/api/clients_test.go) emits
 * the SAME bytes for the same contract, so the two emitters cannot drift on key
 * order, indentation, or the trailing newline.
 */
const SHARED_CONFIG_FIXTURE = join(import.meta.dir, 'fixtures/clientgen/config-with-design.golden.json');
const SHARED_OMIT_FIXTURE = join(import.meta.dir, 'fixtures/clientgen/config-with-omitted-operations.golden.json');

describe('resolveClientGenConfig', () => {
  test('applies documented defaults for an empty input', () => {
    const config = resolveClientGenConfig({}, ctx);
    expect(config).toEqual({
      targets: ['ts'],
      ts: { output: 'clients/ts', packageName: '@demo/widgets-client' },
      go: { output: 'clients/go', modulePath: '', packageName: 'client', clientName: 'Client' },
    });
  });

  test('keeps the default targets array independent of the shared constant', () => {
    const config = resolveClientGenConfig({}, ctx);
    config.targets.push('go');
    // Mutating the resolved config must not corrupt CLIENTGEN_DEFAULTS for the next caller.
    expect(CLIENTGEN_DEFAULTS.targets).toEqual(['ts']);
    expect(resolveClientGenConfig({}, ctx).targets).toEqual(['ts']);
  });

  test('honors explicit overrides for every field', () => {
    const config = resolveClientGenConfig(
      {
        targets: ['ts', 'go'],
        ts: { output: 'out/ts', packageName: '@acme/api' },
        go: {
          output: 'out/go',
          modulePath: 'github.com/acme/api/clients/go',
          packageName: 'api',
          clientName: 'ApiClient',
        },
      },
      ctx,
    );
    expect(config).toEqual({
      targets: ['ts', 'go'],
      ts: { output: 'out/ts', packageName: '@acme/api' },
      go: {
        output: 'out/go',
        modulePath: 'github.com/acme/api/clients/go',
        packageName: 'api',
        clientName: 'ApiClient',
      },
    });
  });

  test('falls back to the computed package name when ts.packageName is absent', () => {
    const config = resolveClientGenConfig({ ts: { output: 'out/ts' } }, ctx);
    expect(config.ts.packageName).toBe('@demo/widgets-client');
    expect(config.ts.output).toBe('out/ts');
  });
});

describe('serializeClientGenConfig', () => {
  test('produces pretty-printed JSON with a trailing newline that round-trips', () => {
    const config: ClientGenConfig = resolveClientGenConfig({}, ctx);
    const json = serializeClientGenConfig(config);
    expect(json.endsWith('\n')).toBe(true);
    expect(JSON.parse(json)).toEqual(config);
  });

  test('is deterministic for identical input', () => {
    expect(serializeClientGenConfig(resolveClientGenConfig({}, ctx))).toBe(
      serializeClientGenConfig(resolveClientGenConfig({}, ctx)),
    );
  });

  test('matches the Go describer byte-for-byte, including per-operation producers', () => {
    const config: ClientGenConfig = {
      ...resolveClientGenConfig(
        {
          targets: ['ts', 'go'],
          go: {
            modulePath: 'github.com/demo/widgets/clients/go',
            packageName: 'widgetsclient',
            clientName: 'WidgetsClient',
          },
        },
        ctx,
      ),
      design: {
        operations: [
          {
            method: 'GET',
            path: '/v1/billing/invoices',
            producerProject: 'acme-platform',
            producerFeature: 'platform/billing',
          },
          {
            method: 'GET',
            path: '/v1/operator/cli-usage',
            producerProject: 'acme-platform',
            producerFeature: 'platform/operator-cli-usage',
          },
        ],
      },
    };
    expect(serializeClientGenConfig(config)).toBe(readFileSync(SHARED_CONFIG_FIXTURE, 'utf8'));
  });

  test('carries go.omitOperations byte-for-byte like the Go describer, and omits it when empty', () => {
    const go = {
      modulePath: 'github.com/demo/widgets/clients/go',
      packageName: 'widgetsclient',
      clientName: 'WidgetsClient',
    };
    const config = resolveClientGenConfig(
      { targets: ['ts', 'go'], go: { ...go, omitOperations: ['deployWidget', 'listWidgets'] } },
      ctx,
    );
    expect(serializeClientGenConfig(config)).toBe(readFileSync(SHARED_OMIT_FIXTURE, 'utf8'));

    const empty = resolveClientGenConfig({ targets: ['ts', 'go'], go: { ...go, omitOperations: [] } }, ctx);
    expect(serializeClientGenConfig(empty)).not.toContain('omitOperations');
  });
});
