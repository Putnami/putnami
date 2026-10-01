import { type Outcome, Repository } from '@putnami/database';
import { DeviceCodes } from '../tables/device-codes';

/**
 * Issues single-use device codes. A code registered in `device_codes` may be
 * redeemed exactly once — the consume-once primitive, backed by a single
 * conditional UPDATE plus Postgres row locking, so the once-only guarantee holds
 * even under racing redemptions and without an enclosing transaction.
 */
export class DeviceCodeService {
  private readonly codes = new Repository(DeviceCodes);

  /**
   * Claim `code` for `userId` exactly once, returning the typed {@link Outcome}:
   * `Applied` on the first redemption, `AlreadyConsumedConflict` on every later
   * one, `NotFound` when the code was never issued.
   */
  async redeem(code: string, userId: string): Promise<Outcome> {
    return this.codes.consumeOnce('code', code, 'consumed = false', 'consumed = true, consumed_by = $1', userId);
  }
}
