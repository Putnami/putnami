import { endpoint } from '@putnami/application';
import { configToken } from '@putnami/runtime';
import { DatabaseConfig } from '../../config';

// GET /database — Primary database configuration
export const GET = endpoint()
  .inject({ config: configToken(DatabaseConfig) })
  .handle((ctx) => ({
    description: 'Primary Database Configuration',
    details: {
      host: ctx.deps.config.host,
      port: ctx.deps.config.port,
      database: ctx.deps.config.name,
      user: ctx.deps.config.user,
    },
  }));
