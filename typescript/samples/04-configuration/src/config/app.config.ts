import { Config, Default } from '@putnami/runtime';

export const FeaturesConfig = Config('app.features', {
  enableCache: Default(Boolean, false),
  enableMetrics: Default(Boolean, false),
});

export const AppConfig = Config('app', {
  name: Default(String, 'My App'),
  version: Default(String, '0.0.0'),
  debug: Default(Boolean, false),
});
