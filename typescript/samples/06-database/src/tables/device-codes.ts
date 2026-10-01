import { Column, Key, Table } from '@putnami/database';

// A once-only device code: redeemable exactly once via Repository.consumeOnce.
// The `consumed` flag is the still-unclaimed guard the claim transitions.
export const DeviceCodes = Table('device_codes', {
  code: Key(String),
  consumed: Column(Boolean, { default: 'false' }),
  consumedBy: Column(String, { columnName: 'consumed_by' }),
});
