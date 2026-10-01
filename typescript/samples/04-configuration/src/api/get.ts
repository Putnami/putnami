import { endpoint } from '@putnami/application';
import { configToken, getEnv } from '@putnami/runtime';
import { ApiConfig, AppConfig, DatabaseConfig, FeaturesConfig, ReplicaDatabaseConfig } from '../config';

// GET / — Overview of all loaded configuration
export const GET = endpoint()
  .inject({
    appConfig: configToken(AppConfig),
    features: configToken(FeaturesConfig),
    primaryDb: configToken(DatabaseConfig),
    replicaDb: configToken(ReplicaDatabaseConfig),
    apiConfig: configToken(ApiConfig),
  })
  .handle((ctx) => {
    const { appConfig, features, primaryDb, replicaDb, apiConfig } = ctx.deps;
    return {
      message: `Welcome to ${appConfig.name}!`,
      environment: getEnv(),
      app: {
        name: appConfig.name,
        version: appConfig.version,
        debug: appConfig.debug,
      },
      features: {
        cache: features.enableCache,
        metrics: features.enableMetrics,
      },
      databases: {
        primary: { host: primaryDb.host, port: primaryDb.port, name: primaryDb.name },
        replica: { host: replicaDb.host, port: replicaDb.port, name: replicaDb.name },
      },
      externalApi: {
        baseUrl: apiConfig.baseUrl,
        timeout: apiConfig.timeout,
        retries: apiConfig.retries,
      },
    };
  });
