/**
 * The command mode: the right to ask another domain to do something.
 *
 * The Go twin is `go.putnami.dev/protocol/architecture/darc/command.go`, and both are held to
 * `protocols/architecture/fixtures/conformance/command-reference-behavior.json`.
 */

import type { ArchitectureImport } from '../architecture/contract.types';
import { ContractError, requireActive, validateContract } from './contract';

/**
 * Deliver one command payload over the declared transport. It is the consumer's
 * carrier — an HTTP call, an event publish, a queue write — because the protocol
 * deliberately does not rank transports.
 */
export type SendFunction<T> = (payload: T) => Promise<void> | void;

/** Construction options. */
export interface CommandOptions {
  /**
   * Receives the error from every {@link Command.emit} that failed. Without one,
   * a fire-and-forget failure is silent, which is what fire-and-forget means and
   * almost never what an operator wants.
   */
  readonly onFailure?: (error: unknown) => void;
}

/** How many sends a command attempted and how many failed. */
export interface CommandStats {
  readonly attempted: number;
  readonly failed: number;
}

/**
 * The right to ask, not the authority to decide.
 *
 * The producer stays the authority over whether it happens; the consumer
 * declares only that it may ask, and over which carrier. What this class
 * enforces is the part a running process can get wrong: it refuses to send at
 * all while the import or its transport is still planned, so a target design
 * cannot quietly become traffic.
 *
 * The two send shapes are the call site's choice, not the contract's — the
 * protocol has no fire-and-forget flag — so they are two methods rather than a
 * mode. {@link Command.send} surfaces the delivery error; {@link Command.emit}
 * does not, which is what an ingest whose failure must never fail the caller
 * needs.
 */
export class Command<T> {
  private readonly declaration: ArchitectureImport;
  private readonly carrier: SendFunction<T>;
  private readonly onFailure?: (error: unknown) => void;
  private attempted = 0;
  private failed = 0;

  /**
   * Build a command from its declared contract.
   *
   * The contract must be ACTIVE, and so must its transport. A planned import is
   * a target: the protocol already forbids it from claiming a current project
   * binding, and a component that sent over it would be making the same claim in
   * code, where no gate reads it.
   */
  constructor(contract: ArchitectureImport, send: SendFunction<T> | undefined, options: CommandOptions = {}) {
    validateContract(contract, 'command');
    if (!send) {
      throw new ContractError(
        contract.id,
        'no carrier was supplied; a command needs the transport its contract declares',
      );
    }
    requireActive(contract, contract.transport);
    this.declaration = contract;
    this.carrier = send;
    this.onFailure = options.onFailure;
  }

  /** The declaration this command enforces. */
  contract(): ArchitectureImport {
    return this.declaration;
  }

  /**
   * Deliver the payload and wait for the carrier's answer. Use it when the
   * caller's own outcome depends on the request being accepted.
   */
  async send(payload: T): Promise<void> {
    this.attempted += 1;
    try {
      await this.carrier(payload);
    } catch (error) {
      this.failed += 1;
      throw new Error(
        `send command ${this.declaration.id} over ${this.declaration.transport?.contract}: ${String(error)}`,
        { cause: error },
      );
    }
  }

  /**
   * Deliver the payload and never report a delivery failure to the caller. It is
   * the shape a contract like "a refused or unreachable ingest never fails a run"
   * needs: the failure reaches the observer, and the caller carries on.
   */
  async emit(payload: T): Promise<void> {
    this.attempted += 1;
    try {
      await this.carrier(payload);
    } catch (error) {
      this.failed += 1;
      this.onFailure?.(
        new Error(
          `emit command ${this.declaration.id} over ${this.declaration.transport?.contract}: ${String(error)}`,
          { cause: error },
        ),
      );
    }
  }

  /**
   * How many sends this command attempted and how many failed. It exists because
   * a fire-and-forget contract is otherwise unobservable from the caller's side,
   * and "we sent nothing all day" and "everything failed" must not look alike.
   */
  stats(): CommandStats {
    return { attempted: this.attempted, failed: this.failed };
  }
}
