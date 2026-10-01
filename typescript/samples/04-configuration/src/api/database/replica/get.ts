import { endpoint } from '@putnami/application';
import { configToken } from '@putnami/runtime';
import { ReplicaDatabaseConfig } from '../../../config';

// GET /database/replica — Replica database (multi-datasource pattern)
export const GET = endpoint()
  .inject({ config: configToken(ReplicaDatabaseConfig) })
  .handle((ctx) => ({
    description: 'Replica Database Configuration (Multi-Datasource Pattern)',
    pattern: 'configToken(ReplicaDatabaseConfig)',
    details: {
      host: ctx.deps.config.host,
      port: ctx.deps.config.port,
      database: ctx.deps.config.name,
      user: ctx.deps.config.user,
    },
  }));
