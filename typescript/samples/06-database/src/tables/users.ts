import { Column, Key, Table } from '@putnami/database';
import { Email, Int, Uuid } from '@putnami/runtime';

export const Users = Table('users', {
  id: Key(Uuid),
  email: Column(Email),
  name: Column(String),
  age: Column(Int),
  createdAt: Column(String, { columnName: 'created_at', default: 'NOW()' }),
});
