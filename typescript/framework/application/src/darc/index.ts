/**
 * Domain Access & Replication Contracts, enforced at run time.
 *
 * A DARC import declares what a consumer promises about facts it does not own:
 * how fresh a copy may be, what happens when it is missing or stale, how updates
 * are ordered and de-duplicated, which component may write it, how it is
 * rebuilt, and how a deletion reaches it. Those promises live in
 * `putnami.architecture.json`, and until a component takes the declaration as
 * its configuration nothing in a running process is obliged to keep them.
 *
 * The classes here close that gap. Construction validates the contract through
 * the architecture protocol — the same verdict `putnami architecture validate`
 * applies — and every read, write, and rebuild is governed by what the contract
 * says. The manifest stops being a description of the code and becomes its
 * configuration.
 *
 * # What this module does not do
 *
 * It moves no data. A projection is given its bootstrap and its updates by the
 * consumer — over whatever carrier the contract declares — and this module
 * governs what happens to them. Choosing an HTTP client, an event subscription,
 * or a file reader stays the consumer's job, because the protocol deliberately
 * does not rank transports.
 *
 * It also decides nothing about permissions. Which domain may import which
 * export is declared and reviewed in a manifest; nothing here reads a workspace,
 * and constructing a component grants no access it was not handed.
 *
 * # Parity
 *
 * The Go twin is `go.putnami.dev/app/darc`, whose components live at
 * `go.putnami.dev/protocol/architecture/darc`. Neither runtime is its own oracle:
 * both execute
 * `protocols/architecture/fixtures/conformance/*-behavior.json`, one corpus of
 * contracts and ordered operations. A behavior that differs between the two
 * fails on one side rather than shipping as two runtimes that describe the same
 * manifest differently.
 *
 * # Status
 *
 * The architecture protocol is experimental: its wire format may change without
 * a migration path, and this module's API is built on its types, so it moves
 * with it.
 */

export {
  ContractError,
  DarcError,
  type DarcErrorCode,
  type DarcRecord,
  type Freshness,
  type ValueCloner,
  type VersionOrder,
  lexicalVersionOrder,
} from './contract';
export { Command, type CommandOptions, type CommandStats, type SendFunction } from './command';
export {
  type BootstrapSource,
  Projection,
  type ProjectionOptions,
  ProjectionWriter,
  type ReplaySource,
  type Update,
} from './projection';
export { Reference } from './reference';
export { Snapshot, type SnapshotOptions } from './snapshot';
export { type DarcComponent, DarcPlugin, evidence } from './evidence';
