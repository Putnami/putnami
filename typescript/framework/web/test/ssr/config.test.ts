import { describe, expect, it } from 'bun:test';
import { PutnamiConfig } from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { PutnamiReactConfig } from '../../src/ssr/react-ssr.config';

describe('PutnamiConfig (shared)', () => {
  it('has correct default values', () => {
    const config = useConfig(PutnamiConfig);

    expect(config.port).toBe(3000);
    expect(config.publicFolder).toBe('public');
    expect(config.skipLoading).toBe(false);
  });
});

describe('PutnamiReactConfig', () => {
  it('has correct default values', () => {
    const config = useConfig(PutnamiReactConfig);

    expect(config.scanFolder).toBe('app');
    expect(config.autoScan).toBe(true);
    expect(config.buildEnable).toBe(true);
    expect(config.sourcemap).toBe('external');
    expect(config.minify).toBe(true);
    expect(config.splitting).toBe(false);
    expect(config.ssrTimeout).toBe(3000);
    expect(config.clientFetchTimeout).toBe(30_000);
    expect(config.moduleCacheSize).toBe(500);
    expect(config.scriptsFolder).toBe('scripts');
  });

  it('isDevelopment defaults based on NODE_ENV', () => {
    const config = useConfig(PutnamiReactConfig);

    // In test environment, NODE_ENV is typically 'test' or undefined
    // so isDevelopment should be true
    expect(config.isDevelopment).toBe(process.env.NODE_ENV !== 'production');
  });

  it('has optional scanPath property', () => {
    const config = useConfig(PutnamiReactConfig);
    expect(config.scanPath).toBeUndefined();
  });

  it('has optional scanRoots property', () => {
    const config = useConfig(PutnamiReactConfig);
    expect(config.scanRoots).toBeUndefined();
  });

  it('allows setting custom values via confInit', () => {
    const config = useConfig(PutnamiReactConfig, {
      confInit: {
        scanFolder: 'custom-app',
        scanRoots: [{ path: 'src/app' }, { path: 'src/projects/web', routePrefix: '/projects' }],
        ssrTimeout: 5000,
        minify: false,
      },
    });

    expect(config.scanFolder).toBe('custom-app');
    expect(config.scanRoots).toEqual([{ path: 'src/app' }, { path: 'src/projects/web', routePrefix: '/projects' }]);
    expect(config.ssrTimeout).toBe(5000);
    expect(config.minify).toBe(false);
  });

  it('allows overriding client fetch timeout and module cache size', () => {
    const config = useConfig(PutnamiReactConfig, {
      confInit: {
        clientFetchTimeout: 5000,
        moduleCacheSize: 50,
      },
    });

    expect(config.clientFetchTimeout).toBe(5000);
    expect(config.moduleCacheSize).toBe(50);
  });
});
