/**
 * The reference mode: a stable handle on facts another domain owns.
 *
 * The Go twin is `go.putnami.dev/protocol/architecture/darc/reference.go`, and both are held to
 * `protocols/architecture/fixtures/conformance/command-reference-behavior.json`.
 */

import type { ArchitectureImport } from '../architecture/contract.types';
import { DarcError, requireActive, validateContract } from './contract';

/**
 * A stable handle on facts another domain owns.
 *
 * It is the thinnest of the five modes and the most common: nothing is copied,
 * so there is no freshness bound, no ordering, and no local model. The consumer
 * keeps an identity and resolves it at the owner.
 *
 * What that leaves to enforce is MINIMIZATION. An import names the exact facts
 * it consumes, and the reason it does is that a fact nobody uses is a permission
 * nobody needed. {@link Reference.fact} refuses a name the contract does not
 * carry, so reaching past the declared surface fails where it happens rather than
 * being discovered later by a reviewer comparing code with a manifest.
 */
export class Reference {
  private readonly declaration: ArchitectureImport;
  private readonly declaredFacts: ReadonlySet<string>;

  /**
   * Build a reference from its declared contract. The contract is validated by
   * the protocol, and the import must be ACTIVE: a planned reference is a target,
   * and resolving one would make the claim in code that the protocol forbids a
   * manifest from making.
   */
  constructor(contract: ArchitectureImport) {
    validateContract(contract, 'reference');
    requireActive(contract, contract.transport);
    this.declaration = contract;
    this.declaredFacts = new Set(contract.facts);
  }

  /** The declaration this reference enforces. */
  contract(): ArchitectureImport {
    return this.declaration;
  }

  /**
   * Whether the import names the fact, and its provenance — the producer export
   * that stays authoritative for it. A name the contract does not carry throws
   * `DarcError('fact-not-imported')`.
   */
  fact(name: string): string {
    if (!this.declaredFacts.has(name)) {
      throw new DarcError(
        'fact-not-imported',
        `darc: the import does not name that fact: import ${this.declaration.id} names ${JSON.stringify(this.facts())}, not ${JSON.stringify(name)}`,
      );
    }
    return this.declaration.from.export;
  }

  /** The minimized fact list the import declares, sorted. */
  facts(): string[] {
    return [...this.declaredFacts].sort();
  }
}
