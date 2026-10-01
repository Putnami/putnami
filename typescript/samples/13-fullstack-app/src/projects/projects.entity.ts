import { Column, Key, Table } from '@putnami/database';
import type { InferTable, SQLDefinition } from '@putnami/database';
import { Uuid } from '@putnami/runtime';

export const projectsMigration: SQLDefinition = {
  name: '001_create_projects',
  sql: `CREATE TABLE IF NOT EXISTS projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    description TEXT DEFAULT '',
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
  );`,
  down: `DROP TABLE IF EXISTS projects;`,
};

export const Projects = Table(
  'projects',
  {
    id: Key(Uuid),
    name: Column(String),
    description: Column(String),
    status: Column(String), // 'active' | 'archived'
    createdAt: Column(String, { columnName: 'created_at', default: 'NOW()' }),
    updatedAt: Column(String, { columnName: 'updated_at', default: 'NOW()' }),
  },
  { db: 'default' },
);

export type Project = InferTable<typeof Projects>;
