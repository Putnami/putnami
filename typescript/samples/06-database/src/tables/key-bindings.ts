import { Column, Key, Table } from '@putnami/database';

// Binds a signing key to a tenant. `tenant` is UNIQUE (a tenant holds at most one
// binding), so binding a successor into an already-bound tenant fails — the
// rotation's second write, which the surrounding unit of work rolls back atomically.
export const KeyBindings = Table('key_bindings', {
  keyId: Key(String, { columnName: 'key_id' }),
  tenant: Column(String),
});
