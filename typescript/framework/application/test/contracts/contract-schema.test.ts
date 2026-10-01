import { describe, expect, it } from 'bun:test';
import { type SchemaDefinition, validateSchema } from '@putnami/runtime';
import { contractStruct, contractType, getContractTypeReference, type ContractManifest } from '../../src/contracts';

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
  it('exposes an endpoint-compatible struct schema without a cast', () => {
    const schema: SchemaDefinition = contractStruct(manifest, 'Request');
    expect(getContractTypeReference(schema)?.name).toBe('Request');
    expect(() => contractStruct(manifest, 'Change')).toThrow('is not a struct');
  });

  it('validates union shape, discriminator, tag, and variant fields', () => {
    const schema = { change: contractType(manifest, 'Change') };
    for (const invalid of ['rename', {}, { kind: 'unknown' }, { kind: 'rename' }, { kind: 'rename', name: 42 }]) {
      expect(validateSchema(schema, { change: invalid }).errors).not.toHaveLength(0);
    }

    expect(validateSchema(schema, { change: { kind: 'rename', name: 'next' } }).errors).toHaveLength(0);
    expect(validateSchema(schema, { change: { kind: 'archive' } }).errors).toHaveLength(0);
  });
});
