import { createHash } from 'node:crypto';
import type { ConfigBlock, FieldSchema } from './config-schema.types';

/**
 * Compute the canonical schema hash matching the Go protocol implementation.
 *
 * Algorithm:
 * 1. Canonicalize blocks recursively (sort by path; sort fields by name at every level;
 *    sort constraints when length > 1; recurse into nested fields, array items, map values).
 * 2. JSON.stringify the canonical form.
 * 3. SHA-256 the JSON bytes.
 * 4. Return "sha256:" + first 16 hex characters.
 */
export function computeSchemaHash(blocks: ConfigBlock[]): string {
  const canonical = canonicalizeBlocks(blocks);
  const json = JSON.stringify(canonical);
  const hex = createHash('sha256').update(json).digest('hex');
  return `sha256:${hex.slice(0, 16)}`;
}

function canonicalizeBlocks(blocks: ConfigBlock[]): ConfigBlock[] {
  return [...blocks]
    .map((b) => ({ path: b.path, fields: canonicalizeFields(b.fields) }))
    .sort((a, c) => a.path.localeCompare(c.path));
}

function canonicalizeFields(fields: FieldSchema[]): FieldSchema[] {
  return fields.map(canonicalizeField).sort((a, c) => a.name.localeCompare(c.name));
}

function canonicalizeField(f: FieldSchema): FieldSchema {
  const out: FieldSchema = { ...f };
  if (out.constraints && out.constraints.length > 1) {
    out.constraints = [...out.constraints].sort();
  }
  if (out.fields && out.fields.length > 0) {
    out.fields = canonicalizeFields(out.fields);
  }
  if (out.items) {
    out.items = canonicalizeField(out.items);
  }
  if (out.values) {
    out.values = canonicalizeField(out.values);
  }
  return out;
}
