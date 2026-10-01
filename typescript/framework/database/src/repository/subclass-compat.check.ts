import type { Outcome } from '../transaction-outcome';
import type { TableDefinition } from '../table';
import { Repository, type RotateSpec } from './repository';

/**
 * Compile-time regression gate: the Repository base class must not declare
 * members that collide with the domain-specific method names subclasses
 * commonly define. A base `Repository.rotate(spec)` would break every consumer
 * subclass with its own `rotate` — e.g. an ApiKeyRepository's
 * `rotate(id, prefix, tokenHash)` — because a TypeScript override must stay
 * assignable to the base member. The optimistic-concurrency
 * primitive is therefore named `rotateRow`, and this file fails `build~types`
 * if a colliding base member is ever (re)introduced. It is type-only coverage:
 * nothing here runs, and nothing is re-exported from the package index.
 */
export class ApiKeyRepositoryCompatCheck extends Repository<TableDefinition> {
  async rotate(id: string, prefix: string, tokenHash: string): Promise<{ id: string; prefix: string }> {
    // The base-class primitive stays reachable under its collision-free name.
    void (this.rotateRow satisfies (spec: RotateSpec) => Promise<Outcome>);
    void tokenHash;
    return { id, prefix };
  }
}
