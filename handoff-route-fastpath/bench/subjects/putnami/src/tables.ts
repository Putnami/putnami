import { Column, DateIso, Int, Key, Table, Uuid } from '@putnami/database';

export const Tasks = Table('tasks', {
  id: Key(Uuid),
  title: Column(String),
  description: Column(String),
  done: Column(Boolean),
  priority: Column(Int),
  createdAt: Column(DateIso, { columnName: 'created_at', default: 'NOW()' }),
});
