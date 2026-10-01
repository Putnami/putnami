import { Column, Key, Table } from '@putnami/database';
import type { InferTable, SQLDefinition } from '@putnami/database';
import { Int, Uuid } from '@putnami/runtime';

export const tasksMigration: SQLDefinition = {
  name: '002_create_tasks',
  sql: `CREATE TABLE IF NOT EXISTS tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title VARCHAR(255) NOT NULL,
    description TEXT DEFAULT '',
    status VARCHAR(20) DEFAULT 'todo',
    priority INTEGER DEFAULT 1,
    assignee VARCHAR(255) DEFAULT '',
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
  );
  CREATE INDEX IF NOT EXISTS idx_tasks_project_id ON tasks(project_id);
  CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);`,
  down: `DROP TABLE IF EXISTS tasks;`,
};

export const Tasks = Table(
  'tasks',
  {
    id: Key(Uuid),
    projectId: Column(Uuid, { columnName: 'project_id' }),
    title: Column(String),
    description: Column(String),
    status: Column(String), // 'todo' | 'in_progress' | 'done'
    priority: Column(Int),
    assignee: Column(String),
    createdAt: Column(String, { columnName: 'created_at', default: 'NOW()' }),
    updatedAt: Column(String, { columnName: 'updated_at', default: 'NOW()' }),
  },
  { db: 'default' },
);

export type Task = InferTable<typeof Tasks>;
