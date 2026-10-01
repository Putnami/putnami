import { Outcome, Repository, runInTransaction } from '@putnami/database';
import { KeyBindings } from '../tables/key-bindings';
import { SigningKeys } from '../tables/signing-keys';

/**
 * Rotates a tenant's signing key as one all-or-nothing unit of work spanning two
 * repositories: `signing_keys` (revoke the predecessor + install the successor,
 * via `rotateRow`) and `key_bindings` (bind the successor to the tenant, via
 * `save`). Wrapping both writes in {@link runInTransaction} is what makes the
 * rotation atomic — `rotateRow` performs two writes and is atomic ONLY inside a
 * transaction, and the binding write joins that same unit.
 */
export class KeyRotationService {
  private readonly keys = new Repository(SigningKeys);
  private readonly bindings = new Repository(KeyBindings);

  /**
   * Retire the active predecessor and install `successorId` for `tenant`,
   * binding the successor, as one unit. Returns:
   *
   *   - `Applied` when the predecessor was revoked, the successor installed, and
   *     the binding written — all committed together;
   *   - `AlreadyConsumedConflict` / `NotFound` when the predecessor could not be
   *     revoked (already rotated, or absent) — the unit installs nothing.
   *
   * A failing second write (e.g. binding into an already-bound tenant) rejects,
   * rolling the whole rotation back: the predecessor stays active and the
   * successor never appears — no transient unbound state.
   */
  async rotate(predecessorId: string, successorId: string, tenant: string): Promise<Outcome> {
    return runInTransaction(async () => {
      const outcome = await this.keys.rotateRow({
        keyColumn: 'id',
        predecessorKey: predecessorId,
        stateColumn: 'state',
        expected: 'active',
        revoked: 'revoked',
        successorColumns: ['id', 'tenant', 'state'],
        successorValues: [successorId, tenant, 'active'],
      });
      if (outcome !== Outcome.Applied) {
        // Predecessor not revoked → nothing installed, nothing to bind. Carry the
        // business outcome out; the empty unit commits (it wrote nothing).
        return outcome;
      }
      // Second repository, SAME unit of work: bind the successor to the tenant. A
      // failure here rolls the signing-key rotation back with it — all-or-nothing.
      await this.bindings.save({ keyId: successorId, tenant });
      return outcome;
    });
  }
}
