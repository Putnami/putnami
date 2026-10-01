import { Config, Default, Env, Int, Optional } from '@putnami/runtime';

export const PutnamiConfig = Config('putnami', {
  port: Default(Int, 3000),
  publicFolder: Default(String, 'public'),
  skipLoading: Default(Boolean, false),
  /**
   * Override the directory from which static assets and scripts are served.
   * Defaults to `<projectRoot>/.gen/<publicFolder>`.
   * Set this for bundled/Docker builds where assets are co-located with the bundle.
   * Can be set via the `PUTNAMI_ASSETS_DIR` environment variable.
   */
  assetsDir: Optional(Env('PUTNAMI_ASSETS_DIR', String)),
});
