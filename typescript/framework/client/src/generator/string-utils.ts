/**
 * Shared string utilities for code generation.
 */

import { createHash } from 'node:crypto';

/**
 * Compute a truncated SHA-256 hash of content for spec drift detection.
 *
 * Build-time only — uses Node's `crypto` (available in Bun and Node) rather than
 * the Bun-only `Bun.CryptoHasher`. Produces the same SHA-256 digest as the
 * runtime's Web Crypto implementation in `runtime/spec-hash.ts`, so the embedded
 * hash and the live drift-check hash stay comparable.
 */
export function computeSpecHash(content: string): string {
  return createHash('sha256').update(content).digest('hex').substring(0, 16);
}

/**
 * Convert the first character to lowercase (assumes input is PascalCase).
 */
export function toCamelCase(str: string): string {
  return str.charAt(0).toLowerCase() + str.slice(1);
}

/**
 * Derive a JavaScript symbol from a canonical operation id.
 *
 * A canonical operation id is contract identity and may legally carry
 * punctuation an identifier cannot (`getV1_Operator_Cli-usage`), so every
 * character outside the identifier alphabet becomes `_` and a leading digit is
 * prefixed with `_`. The canonical id is kept verbatim in the generated
 * descriptor and in the runtime trace, so lineage survives this rewrite.
 *
 * The mapping is deliberately not injective (`a-b` and `a_b` both normalize to
 * `a_b`); the generator rejects the resulting duplicate method names rather than
 * inventing a disambiguating suffix that would be unstable across spec edits.
 */
export function toSafeIdentifier(name: string): string {
  const normalized = name.replace(/[^A-Za-z0-9_$]/g, '_');
  return /^[0-9]/.test(normalized) ? `_${normalized}` : normalized;
}

/**
 * Convert a string to PascalCase by splitting on hyphens and underscores.
 */
export function pascalCase(str: string): string {
  return str
    .split(/[-_]/)
    .map((s) => s.charAt(0).toUpperCase() + s.slice(1))
    .join('');
}

/**
 * Derive the PascalCase base of a per-operation type name
 * (`${base}Path`, `${base}Result`, `${base}NotFoundError`, ...) from a
 * canonical operation id.
 *
 * A canonical operation id is contract identity and may legally carry
 * punctuation `pascalCase` does not split on (`get.well-known_Putnami_Events`),
 * so every character outside the identifier alphabet is treated as a word
 * break here, and a leading digit is prefixed with `_`. The operation id
 * itself is kept verbatim wherever it is emitted as a literal (the
 * descriptor's `operationId: JSON.stringify(...)`); only this derived type
 * name changes. openapi-reader.ts's `inferServiceName` applies the same rule
 * to service names. Go's `upperCamelClient`
 * (go/framework/api/clientgen.go) splits on `-`, `_`, `.` and space only, with
 * no digit guard and no fallback, so the two agree on canonical ids such as
 * `get.well-known_Putnami_Events` but not on every input.
 *
 * The mapping is not injective (`get_a` and `get.a`, `a$b` and `ab` share a
 * base); the emitters refuse the resulting duplicate type names.
 */
export function operationTypeBase(operationId: string): string {
  const name = pascalCase(operationId.replace(/[^A-Za-z0-9]+/g, '-')) || 'Operation';
  return /^[0-9]/.test(name) ? `_${name}` : name;
}

/**
 * Convert snake_case to camelCase.
 */
export function snakeToCamel(str: string): string {
  return str.replace(/_([a-z])/g, (_, c) => c.toUpperCase());
}

/**
 * Convert PascalCase/camelCase to kebab-case.
 */
export function toKebabCase(str: string): string {
  return str
    .replace(/([a-z])([A-Z])/g, '$1-$2')
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1-$2')
    .toLowerCase();
}

/**
 * The shape a spec-derived name must have before a generator may interpolate
 * it into generated source as an identifier (class / method / interface-member
 * / property-access position). Deliberately ASCII-only: exotic-but-technically
 * -valid identifier characters are rejected rather than reasoned about.
 */
const SAFE_IDENTIFIER = /^[A-Za-z_$][A-Za-z0-9_$]*$/;

/**
 * Names that pass the identifier regex but must never be emitted as generated
 * members or keys: they collide with `Object.prototype` machinery (prototype
 * pollution) or the class constructor slot.
 */
const FORBIDDEN_IDENTIFIERS = new Set(['__proto__', 'constructor', 'prototype']);

/**
 * Assert that a spec-derived name is safe to interpolate into generated code
 * as an identifier.
 *
 * Specs (OpenAPI / proto) are untrusted input: a field, parameter, query,
 * operationId, or schema name such as `a }); sideEffect(); ({b` would
 * otherwise splice executable code into the generated client. String LITERALS
 * are escaped by the generators' literal sanitizers; identifier positions
 * cannot be escaped, so anything that is not a plain JS identifier is rejected
 * at generation time with an error naming the offender and where it came from.
 *
 * @param name spec-derived identifier candidate
 * @param context where the name came from, for the error message
 *   (e.g. `query parameter of operation "listUsers"`)
 */
export function assertSafeIdentifier(name: string, context: string): void {
  if (!SAFE_IDENTIFIER.test(name)) {
    throw new Error(
      `Unsafe ${context}: ${JSON.stringify(name)} is not a valid JavaScript identifier. ` +
        'Spec-derived identifiers must match /^[A-Za-z_$][A-Za-z0-9_$]*$/ so an untrusted spec cannot inject code into the generated client.',
    );
  }
  if (FORBIDDEN_IDENTIFIERS.has(name)) {
    throw new Error(
      `Unsafe ${context}: ${JSON.stringify(name)} is forbidden in generated code (prototype-pollution / constructor-clobbering risk).`,
    );
  }
}
