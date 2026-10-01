import { Column, Key, Table } from '@putnami/database';

// A tenant signing key. Rotation transitions `state` active → revoked on the
// predecessor and inserts a fresh active successor — both writes in one unit.
export const SigningKeys = Table('signing_keys', {
  id: Key(String),
  tenant: Column(String),
  state: Column(String),
});
