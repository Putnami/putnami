/**
 * Collector access — the bearer of each push and the warnings a collector's
 * answers raise, shared by the telemetry plugin and the OTLP log sink.
 */

import { getTelemetryTokenSource, useLogger } from '@putnami/runtime';
import type { OtlpSendResult } from './telemetry.sender';

/** The bearer of one push, or `ok: false` when the batch must be dropped. */
export type PushCredential = { ok: true; bearer?: string } | { ok: false };

/**
 * Resolves the bearer of each push to one collector and logs the first
 * credential failure and the first refusal; later ones stay silent.
 */
export class CollectorAccess {
  private refusalWarned = false;
  private credentialWarned = false;

  /**
   * @param endpoint The collector base URL the token source is asked about.
   * @param bearer A static bearer; it wins over the registered token source.
   */
  constructor(
    readonly endpoint: string,
    private readonly bearer?: string,
  ) {}

  /**
   * The bearer of one push: the static bearer, else the registered token
   * source's answer, else none. A failing source answers `ok: false`: nothing
   * is sent without a credential.
   */
  async credential(): Promise<PushCredential> {
    if (this.bearer) return { ok: true, bearer: this.bearer };
    const source = getTelemetryTokenSource();
    if (!source) return { ok: true };
    try {
      const bearer = await source(this.endpoint);
      return bearer ? { ok: true, bearer } : { ok: true };
    } catch {
      if (!this.credentialWarned) {
        this.credentialWarned = true;
        useLogger('telemetry').warn(
          'telemetry could not obtain a credential for its collector; the batch was dropped. Later failures are not logged.',
          { endpoint: this.endpoint },
        );
      }
      return { ok: false };
    }
  }

  /** Log the first refusal (401 or 403) by the collector; later ones stay silent. */
  observe(result: OtlpSendResult): void {
    if ((result.status === 401 || result.status === 403) && !this.refusalWarned) {
      this.refusalWarned = true;
      useLogger('telemetry').warn(
        `the telemetry collector refused a push (HTTP ${result.status}); check the credential its token source supplies. Later refusals are not logged.`,
        { endpoint: this.endpoint },
      );
    }
  }
}
