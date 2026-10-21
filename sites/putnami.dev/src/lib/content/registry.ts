/**
 * Minimal put-registry client for site-content bundles.
 *
 * There is no TypeScript put-registry client in the workspace, so this module
 * implements exactly the two calls the content pipeline needs, mirroring the
 * Go CLI conventions:
 *
 * - URL trust: https:// always; http:// only for loopback hosts or under
 *   `PUTNAMI_ALLOW_INSECURE_REGISTRY=1` (tooling/cli registry_url.go).
 * - Auth: the `registry-token/v1` seam (protocols/registry) — spawn
 *   `putnami cloud registry-token --host <host>` without a shell and without
 *   blocking the event loop, with `PUTNAMI_NO_AUTO_INSTALL=1` (registrycred);
 *   empty stdout, a non-zero exit, a non-bearer value, a missing CLI, or a
 *   child stopped by its time or stdout bound all mean "no token, proceed
 *   anonymously".
 * - Package naming: `RegistryRef` semantics — "site-content/cloud" →
 *   namespace "site-content", package "cloud"; unscoped names default to the
 *   "putnami" namespace.
 *
 * Wire endpoints (v1 consumption convention, exercised in production only once
 * the Cloud workspace publishes a real bundle — until then the lock is empty and
 * tests substitute doubles through the `fetchBlob` seam):
 *
 * - fetch by digest:   GET {registry}/{ns}/{pkg}/blobs/{digest}
 * - resolve a channel: the channel pointer below, then fetch by its digest
 *   (the digest is always COMPUTED from the fetched bytes; `/download` is the
 *   put binary route and refuses a request without `os` and `arch`)
 * - channel pointer:   GET {registry}/{ns}/{pkg}/channels/{channel}
 *   (cheap JSON `{ "version", "digest" }` — the runtime overlay's TTL poll;
 *   never downloads the blob, the advertised digest is later re-verified
 *   against the fetched bytes)
 */
import type { LockBundle } from './lock';
import { validDigest } from './sitecontent';

/** Opt-in for plaintext http registries (mirrors the Go CLI env). */
export const ALLOW_INSECURE_REGISTRY_ENV = 'PUTNAMI_ALLOW_INSECURE_REGISTRY';

/** Upper bound on a bundle blob; documentation payloads are far smaller. */
export const MAX_BUNDLE_BLOB_BYTES = 256 * 1024 * 1024;

/** Upper bound on a channel-pointer document; it is a tiny JSON object. */
export const MAX_CHANNEL_POINTER_BYTES = 64 * 1024;

export interface RegistryRef {
  namespace: string;
  pkg: string;
}

/** "@ns/pkg" | "ns/pkg" | "pkg" → registry namespace + package. */
export function registryRef(name: string): RegistryRef {
  const trimmed = name.startsWith('@') ? name.slice(1) : name;
  const slash = trimmed.indexOf('/');
  if (slash >= 0) return { namespace: trimmed.slice(0, slash), pkg: trimmed.slice(slash + 1) };
  return { namespace: 'putnami', pkg: trimmed };
}

function isLoopbackHost(host: string): boolean {
  const h = host.toLowerCase();
  return h === 'localhost' || h === '127.0.0.1' || h === '::1' || h === '[::1]';
}

/**
 * Reject registry URLs that could downgrade the fetch to a MITM-able channel:
 * https:// always passes; http:// only for loopback hosts or with the explicit
 * `PUTNAMI_ALLOW_INSECURE_REGISTRY=1` opt-in. Anything else throws.
 */
export function validateRegistryUrl(raw: string): URL {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new Error(`invalid registry URL ${JSON.stringify(raw)}`);
  }
  if (url.protocol === 'https:') return url;
  if (url.protocol === 'http:') {
    if (isLoopbackHost(url.hostname) || process.env[ALLOW_INSECURE_REGISTRY_ENV] === '1') return url;
    throw new Error(
      `registry URL ${JSON.stringify(raw)} uses plaintext http://; refusing to fetch over an ` +
        `unauthenticated channel. Switch to https://, or set ${ALLOW_INSECURE_REGISTRY_ENV}=1 to override`,
    );
  }
  throw new Error(`registry URL ${JSON.stringify(raw)} has unsupported scheme (only https and http are accepted)`);
}

/** Exit code and stdout of one command run. */
export interface CommandResult {
  exitCode: number;
  stdout: string;
}

/**
 * Runs a command; the seam tests stub. A runner may answer synchronously or
 * with a promise: callers always await it.
 */
export type CommandRunner = (argv: string[]) => CommandResult | Promise<CommandResult>;

/** Upper bound on one registry-token child, matching the Go seam (registrycred cloudTokenTimeout). */
export const REGISTRY_TOKEN_TIMEOUT_MS = 30_000;

/**
 * Delay between SIGTERM and SIGKILL for a child that is still running, and the
 * longest wait for its exit after SIGKILL.
 */
export const REGISTRY_TOKEN_KILL_GRACE_MS = 2000;

/**
 * Set to `1` in the registry-token child's environment, as the Go seam does
 * (registrycred): the child mints one bearer and never runs a first-use install
 * that would print on stdout ahead of the bearer.
 */
export const NO_AUTO_INSTALL_ENV = 'PUTNAMI_NO_AUTO_INSTALL';

/** Upper bound on registry-token stdout; a bearer is far smaller. */
export const MAX_REGISTRY_TOKEN_STDOUT_BYTES = 64 * 1024;

/** The result every failed run maps to: the caller then proceeds anonymously. */
const FAILED_RUN: CommandResult = { exitCode: 1, stdout: '' };

export interface SpawnRunnerOptions {
  /** Time after which the child is stopped and the run counts as failed. */
  timeoutMs?: number;
  /** Delay between SIGTERM and SIGKILL, and the longest wait for the exit after SIGKILL. */
  killGraceMs?: number;
  /** Stdout bytes beyond which the child is stopped and the run counts as failed. */
  maxStdoutBytes?: number;
}

/**
 * Read a stream to its end. Returns the bytes, or `null` once `cap` is passed;
 * `cap` of `Infinity` drains and discards. Each chunk is awaited, so the event
 * loop keeps serving while the child writes.
 */
async function drainStream(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  cap: number,
  keep: boolean,
): Promise<Uint8Array | null> {
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.length;
    if (total > cap) return null;
    if (keep) chunks.push(value);
  }
  const out = new Uint8Array(keep ? total : 0);
  let at = 0;
  for (const chunk of chunks) {
    out.set(chunk, at);
    at += chunk.length;
  }
  return out;
}

/** The part of a spawned child that {@link stopChild} uses. */
interface RunningChild {
  readonly exited: Promise<number>;
  readonly exitCode: number | null;
  readonly signalCode: string | null;
  kill(signal?: number | NodeJS.Signals): void;
}

/** Resolves true once `child` has exited, or false after `ms`. */
async function exitsWithin(child: RunningChild, ms: number): Promise<boolean> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const elapsed = new Promise<false>((resolve) => {
    timer = setTimeout(() => resolve(false), ms);
  });
  try {
    return await Promise.race([child.exited.then(() => true), elapsed]);
  } finally {
    clearTimeout(timer);
  }
}

function sendSignal(child: RunningChild, signal: NodeJS.Signals): void {
  try {
    child.kill(signal);
  } catch {
    // A child that is already gone needs no signal.
  }
}

/**
 * Stop a child that is still running: SIGTERM, then SIGKILL once `graceMs`
 * passes, then wait at most `graceMs` more for its exit. Never throws, and
 * returns within `2 × graceMs`. A child that has already exited is left alone.
 */
async function stopChild(child: RunningChild, graceMs: number): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  sendSignal(child, 'SIGTERM');
  if (await exitsWithin(child, graceMs)) return;
  sendSignal(child, 'SIGKILL');
  await exitsWithin(child, graceMs);
}

/**
 * A runner that spawns `argv` without blocking the event loop. The child
 * inherits this process's environment plus `PUTNAMI_NO_AUTO_INSTALL=1`. stdout
 * and stderr are drained while the child runs; stderr is dropped, never logged.
 *
 * A run fails, and maps to {@link FAILED_RUN}, when `timeoutMs` passes, when
 * stdout passes `maxStdoutBytes`, or when a pipe read fails. A spawn that
 * throws (a missing executable) rejects instead, before any child exists. A
 * failed child still running gets SIGTERM, then SIGKILL after `killGraceMs`.
 * The returned promise settles within `timeoutMs + 2 × killGraceMs` plus
 * scheduling: the second grace bounds the wait for the exit after SIGKILL. The
 * pipe readers are then cancelled, so a grandchild that keeps a pipe open
 * cannot hold the run.
 *
 * The child stays in this process's group, so a supervisor that stops the
 * group also stops the child.
 */
export function spawnCommandRunner(options: SpawnRunnerOptions = {}): CommandRunner {
  const timeoutMs = options.timeoutMs ?? REGISTRY_TOKEN_TIMEOUT_MS;
  const killGraceMs = options.killGraceMs ?? REGISTRY_TOKEN_KILL_GRACE_MS;
  const maxStdoutBytes = options.maxStdoutBytes ?? MAX_REGISTRY_TOKEN_STDOUT_BYTES;

  return async (argv: string[]): Promise<CommandResult> => {
    // argv-form spawn of a fixed command: the host travels as a discrete
    // argument, no shell is involved, so it cannot inject. A missing
    // executable throws here, before any resource exists; the caller maps
    // that to anonymous.
    const proc = Bun.spawn(argv, {
      env: { ...process.env, [NO_AUTO_INSTALL_ENV]: '1' },
      stdin: 'ignore',
      stdout: 'pipe',
      stderr: 'pipe',
    });
    const stdoutReader = proc.stdout.getReader();
    const stderrReader = proc.stderr.getReader();

    // Settles once the deadline passes or stdout passes its cap.
    let fail!: () => void;
    const failed = new Promise<'failed'>((resolve) => {
      fail = () => resolve('failed');
    });
    const deadline = setTimeout(() => fail(), timeoutMs);
    const stdoutDone = drainStream(stdoutReader, maxStdoutBytes, true).then((bytes) => {
      if (bytes === null) fail();
      return bytes;
    });
    const stderrDone = drainStream(stderrReader, Number.POSITIVE_INFINITY, false);

    try {
      const outcome = await Promise.race([Promise.all([proc.exited, stdoutDone, stderrDone]), failed]);
      if (outcome === 'failed') return FAILED_RUN;
      const [, stdout] = outcome;
      if (stdout === null) return FAILED_RUN;
      return { exitCode: proc.exitCode ?? 1, stdout: new TextDecoder().decode(stdout) };
    } catch {
      return FAILED_RUN;
    } finally {
      clearTimeout(deadline);
      await stopChild(proc, killGraceMs);
      // Releases a pipe a surviving grandchild still holds; no-op once drained.
      await Promise.allSettled([stdoutReader.cancel(), stderrReader.cancel()]);
    }
  };
}

const runPutnamiCommand: CommandRunner = spawnCommandRunner();

/**
 * Resolve a bearer for `host` through the registry-token/v1 seam. Any failure
 * mode (cloud absent, unmanaged host, signed out, non-bearer stdout, a child
 * that times out) yields undefined and the fetch proceeds anonymously — the
 * digest check downstream is what protects integrity, tokens only gate access.
 * The token is never logged.
 */
export async function resolveRegistryToken(
  host: string,
  run: CommandRunner = runPutnamiCommand,
): Promise<string | undefined> {
  if (host === '') return undefined;
  let result: CommandResult;
  try {
    result = await run(['putnami', 'cloud', 'registry-token', '--host', host]);
  } catch {
    return undefined;
  }
  if (result.exitCode !== 0) return undefined;
  const token = result.stdout.trim();
  // Bare-bearer contract: a value with internal whitespace is a status line on
  // the wrong stream, not a token (protocols/registry ValidBearer).
  if (token === '' || /\s/.test(token)) return undefined;
  return token;
}

async function readCappedBody(response: Response, cap: number, what: string): Promise<Uint8Array> {
  const declared = Number.parseInt(response.headers.get('content-length') ?? '', 10);
  if (Number.isSafeInteger(declared) && declared > cap) {
    throw new Error(`${what}: response of ${declared} bytes exceeds the ${cap}-byte limit`);
  }
  if (!response.body) return new Uint8Array(0);
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.length;
    if (total > cap) {
      await reader.cancel();
      throw new Error(`${what}: response exceeds the ${cap}-byte limit`);
    }
    chunks.push(value);
  }
  const out = new Uint8Array(total);
  let at = 0;
  for (const chunk of chunks) {
    out.set(chunk, at);
    at += chunk.length;
  }
  return out;
}

async function registryGet(url: URL, token: string | undefined, what: string): Promise<Response> {
  const headers: Record<string, string> = {};
  if (token !== undefined) headers['authorization'] = `Bearer ${token}`;
  const response = await fetch(url, { headers, redirect: 'follow' });
  if (!response.ok) {
    throw new Error(`${what}: HTTP ${response.status}`);
  }
  return response;
}

/**
 * Fetch a bundle blob BY DIGEST. The caller must still verify sha256(blob)
 * against the lock digest — the content address, not this transport, is the
 * integrity authority.
 */
export async function fetchBundleBlobFromRegistry(
  bundle: Pick<LockBundle, 'package' | 'digest' | 'registry'>,
  run: CommandRunner = runPutnamiCommand,
): Promise<Uint8Array> {
  if (!validDigest(bundle.digest)) {
    throw new Error(`refusing to fetch non-digest reference ${JSON.stringify(bundle.digest)}`);
  }
  const base = validateRegistryUrl(bundle.registry);
  const { namespace, pkg } = registryRef(bundle.package);
  const url = new URL(
    `${encodeURIComponent(namespace)}/${encodeURIComponent(pkg)}/blobs/${bundle.digest}`,
    base.href.endsWith('/') ? base.href : `${base.href}/`,
  );
  const token = await resolveRegistryToken(base.host, run);
  const what = `fetch bundle blob ${bundle.package}@${bundle.digest.slice(0, 12)}`;
  const response = await registryGet(url, token, what);
  return readCappedBody(response, MAX_BUNDLE_BLOB_BYTES, what);
}

export interface ResolvedChannel {
  /** Version the registry advertised for the channel (provenance). */
  version: string;
  /** The bundle blob itself; the caller computes the digest from these bytes. */
  blob: Uint8Array;
}

/**
 * Resolve `package@channel` → the current blob + advertised version. This is
 * the ONLY channel-resolution call in the content pipeline and it is reserved
 * for `content bump` — generate must never call it (a determinism constraint).
 */
export async function resolveChannelFromRegistry(
  packageName: string,
  channel: string,
  registry: string,
  run: CommandRunner = runPutnamiCommand,
): Promise<ResolvedChannel> {
  // The pointer refuses an empty version, so the lock never gets an
  // unparseable entry (parseContentLock rejects ""). Its digest only selects
  // which blob to fetch: the caller still computes the digest from the bytes.
  const pointer = await fetchChannelPointerFromRegistry(packageName, channel, registry, run);
  const blob = await fetchBundleBlobFromRegistry({ package: packageName, digest: pointer.digest, registry }, run);
  return { version: pointer.version, blob };
}

/** The current head of a channel as advertised by the registry. */
export interface ChannelPointer {
  /** Version the registry advertises for the channel (provenance). */
  version: string;
  /** sha256 bare-hex of the channel-head blob. Advisory until the fetched
   * bytes are verified against it — the content address stays the integrity
   * authority, this pointer only decides WHETHER to fetch. */
  digest: string;
}

/**
 * Fetch the CHEAP channel pointer for `package@channel` — a tiny JSON document
 * `{ "version": string, "digest": string }` — without downloading the bundle
 * blob. This is the runtime overlay's TTL poll; `content bump` goes
 * through {@link resolveChannelFromRegistry}, which then fetches the blob.
 */
export async function fetchChannelPointerFromRegistry(
  packageName: string,
  channel: string,
  registry: string,
  run: CommandRunner = runPutnamiCommand,
): Promise<ChannelPointer> {
  const base = validateRegistryUrl(registry);
  const { namespace, pkg } = registryRef(packageName);
  const url = new URL(
    `${encodeURIComponent(namespace)}/${encodeURIComponent(pkg)}/channels/${encodeURIComponent(channel)}`,
    base.href.endsWith('/') ? base.href : `${base.href}/`,
  );
  const token = await resolveRegistryToken(base.host, run);
  const what = `resolve channel pointer ${packageName}@${channel}`;
  const response = await registryGet(url, token, what);
  const body = await readCappedBody(response, MAX_CHANNEL_POINTER_BYTES, what);

  let doc: unknown;
  try {
    doc = JSON.parse(new TextDecoder().decode(body));
  } catch (error) {
    throw new Error(`${what}: response is not valid JSON: ${(error as Error).message}`);
  }
  if (typeof doc !== 'object' || doc === null || Array.isArray(doc)) {
    throw new Error(`${what}: response must be a JSON object`);
  }
  const record = doc as Record<string, unknown>;
  const version = typeof record['version'] === 'string' ? record['version'].trim() : '';
  const digest = typeof record['digest'] === 'string' ? record['digest'] : '';
  if (version === '') {
    throw new Error(`${what}: pointer advertises no version`);
  }
  if (!validDigest(digest)) {
    throw new Error(`${what}: pointer digest ${JSON.stringify(digest)} must be sha256 bare-hex`);
  }
  return { version, digest };
}
