import { describe, expect } from 'bun:test';
import { type SchemaDefinition, validateSchema } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { contractStruct, contractType, getContractTypeReference, type ContractManifest } from '../../src/contracts';

// Each test proves a requirement of the contracts protocol's feature under a check of this package's own.
const UNION_VALIDATION = {
  feature: 'contracts/contract-ir',
  requirement: 'contract-types-become-components',
  check: 'the-typescript-runtime-validates-contract-unions',
};

const manifest: ContractManifest = {
  protocolVersion: 1,
  name: 'example/runtime-contract',
  unions: [
    {
      name: 'Change',
      discriminator: 'kind',
      variants: [
        { tag: 'rename', fields: [{ name: 'name', type: 'string' }] },
        { tag: 'archive', fields: [{ name: 'reason', type: 'string', optional: true }] },
      ],
    },
  ],
  structs: [{ name: 'Request', fields: [{ name: 'change', type: 'Change' }] }],
};

describe('contract runtime schemas', () => {
  specTest('exposes an endpoint-compatible struct schema without a cast', UNION_VALIDATION, () => {
    const schema: SchemaDefinition = contractStruct(manifest, 'Request');
    expect(getContractTypeReference(schema)?.name).toBe('Request');
    expect(() => contractStruct(manifest, 'Change')).toThrow('is not a struct');
  });

  specTest('validates union shape, discriminator, tag, and variant fields', UNION_VALIDATION, () => {
    const schema = { change: contractType(manifest, 'Change') };
    for (const invalid of ['rename', {}, { kind: 'unknown' }, { kind: 'rename' }, { kind: 'rename', name: 42 }]) {
      expect(validateSchema(schema, { change: invalid }).errors).not.toHaveLength(0);
    }

    expect(validateSchema(schema, { change: { kind: 'rename', name: 'next' } }).errors).toHaveLength(0);
    expect(validateSchema(schema, { change: { kind: 'archive' } }).errors).toHaveLength(0);
  });
});
