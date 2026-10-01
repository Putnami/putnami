/**
 * Bundle-archive verification: the consumer half of `site-content-bundle/v1`.
 *
 * The put-registry blob a lock entry pins is a single tar.gz whose entries are
 * the reserved `bundle.json` manifest at the archive root plus every payload
 * file. Addressing manifest and payload with ONE digest keeps the manifest
 * itself tamper-proof under the lock's content address (a separately fetched
 * manifest would escape the pin).
 *
 * Payload rules are the exact `protocols/sitecontent` VerifyPayload semantics
 * (payload.go): regular files only, no symlinks/hardlinks, safe relative paths,
 * every file under a declared mount, listed exactly once with a matching
 * sha256, and every listed file present. `bundle.json` is the manifest carrier,
 * not a payload file, so it is exempt from the listed/mount checks.
 */
import { createHash } from 'node:crypto';
import {
  type Diagnostic,
  ErrorCodes,
  FORMAT_MAJOR,
  formatVersionMajor,
  hasErrors,
  manifestFormatVersion,
  type Manifest,
  mountForPath,
  parseManifest,
  urlPath,
  validateManifest,
  validRelPath,
} from './sitecontent';
import { readTarEntries, type TarEntry } from './tar';

/** Reserved archive entry carrying the manifest. */
export const MANIFEST_ENTRY_NAME = 'bundle.json';

export interface VerifiedBundle {
  manifest: Manifest;
  /** Payload file contents keyed by payload-relative path. */
  files: Map<string, Uint8Array>;
}

export interface VerifyArchiveResult {
  bundle: VerifiedBundle | null;
  diagnostics: Diagnostic[];
  /**
   * True when the manifest parsed but declares a NEWER format major than this
   * consumer implements. The contract mandates the consumer ignore such a
   * bundle and fall back to baked content — in every environment, CI included —
   * so callers must treat this as skip-with-warning, never as a failure.
   */
  newerFormatMajor: boolean;
}

export function sha256Hex(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex');
}

function diag(code: string, field: string, message: string): Diagnostic {
  return { code, field, message };
}

function fail(diagnostics: Diagnostic[]): VerifyArchiveResult {
  return { bundle: null, diagnostics, newerFormatMajor: false };
}

function gunzip(blob: Uint8Array): Uint8Array | null {
  try {
    // Copy into a fresh ArrayBuffer-backed view: Bun's gunzipSync typing
    // rejects ArrayBufferLike-backed views (e.g. Buffer slices). Retested on
    // Bun 1.4.0: @types/bun still requires Uint8Array<ArrayBuffer>.
    return Bun.gunzipSync(new Uint8Array(blob));
  } catch {
    return null;
  }
}

/**
 * Gunzip + untar a digest-verified bundle blob, inspect its format-version
 * envelope, gate on a newer major before decoding its unknown shape, then
 * strict-parse and validate a readable manifest and verify every payload entry
 * against it. Returns the decoded files only when there are no errors.
 */
export function verifyBundleArchive(blob: Uint8Array): VerifyArchiveResult {
  const tarBytes = gunzip(blob);
  if (tarBytes === null) {
    return fail([diag(ErrorCodes.ParseError, '', 'invalid tar.gz payload: not a gzip stream')]);
  }
  let entries: TarEntry[];
  try {
    entries = readTarEntries(tarBytes);
  } catch (error) {
    return fail([diag(ErrorCodes.ParseError, '', `invalid tar payload: ${(error as Error).message}`)]);
  }

  const manifestEntry = entries.find((e) => e.name === MANIFEST_ENTRY_NAME);
  if (manifestEntry?.typeflag !== '0') {
    return fail([
      diag(ErrorCodes.ParseError, MANIFEST_ENTRY_NAME, 'bundle archive must carry a regular bundle.json at its root'),
    ]);
  }

  // Read only the stable version envelope before strict decoding: a newer
  // major may carry fields and shapes this parser cannot judge. Invalid
  // envelopes never trigger fallback; strict parsing below rejects them.
  const formatVersion = manifestFormatVersion(manifestEntry.data);
  const major = formatVersion === null ? null : formatVersionMajor(formatVersion);
  if (major !== null && major > FORMAT_MAJOR) {
    return {
      bundle: null,
      diagnostics: [
        diag(
          ErrorCodes.InvalidFormatVersion,
          'formatVersion',
          `formatVersion ${JSON.stringify(formatVersion)} is not readable by this parser ` +
            `(want major ${FORMAT_MAJOR})`,
        ),
      ],
      newerFormatMajor: true,
    };
  }

  const parsed = parseManifest(manifestEntry.data);
  if (parsed.manifest === null) return fail(parsed.diagnostics);
  const manifest = parsed.manifest;

  const diags = validateManifest(manifest);
  if (hasErrors(diags)) return fail(diags);

  diags.push(...verifyPayloadEntries(manifest, entries, MANIFEST_ENTRY_NAME));
  if (hasErrors(diags)) return fail(diags);

  const files = new Map<string, Uint8Array>();
  for (const entry of entries) {
    if (entry.name !== MANIFEST_ENTRY_NAME) files.set(entry.name, entry.data);
  }
  return { bundle: { manifest, files }, diagnostics: [], newerFormatMajor: false };
}

/**
 * Payload verification against a validated manifest — a faithful port of Go
 * VerifyPayload, parameterized by the reserved manifest entry name.
 */
export function verifyPayloadEntries(manifest: Manifest, entries: TarEntry[], reservedName?: string): Diagnostic[] {
  const diags: Diagnostic[] = [];
  const listed = new Map<string, string>(manifest.files.map((f) => [f.path, f.digest]));
  const seen = new Set<string>();

  for (const entry of entries) {
    if (reservedName !== undefined && entry.name === reservedName) continue;
    const path = entry.name;
    if (entry.typeflag === '2' || entry.typeflag === '1') {
      diags.push(
        diag(
          ErrorCodes.InvalidSymlink,
          path,
          `payload entry ${JSON.stringify(path)} is a link; symlinks and hardlinks are not allowed`,
        ),
      );
      continue;
    }
    if (entry.typeflag !== '0') {
      diags.push(
        diag(
          ErrorCodes.InvalidPayloadEntry,
          path,
          `payload entry ${JSON.stringify(path)} has unsupported tar type ${JSON.stringify(entry.typeflag)}; ` +
            `only regular files are allowed`,
        ),
      );
      continue;
    }
    if (!validRelPath(path)) {
      diags.push(
        diag(
          ErrorCodes.InvalidPath,
          path,
          `payload path ${JSON.stringify(path)} must be a forward-slash relative path with no traversal`,
        ),
      );
      continue;
    }
    if (!mountForPath(manifest.mounts, path)) {
      diags.push(
        diag(
          ErrorCodes.PathOutsideMount,
          path,
          `payload path ${JSON.stringify(path)} (served at ${JSON.stringify(urlPath(path))}) ` +
            `falls under no declared mount`,
        ),
      );
      continue;
    }
    const expected = listed.get(path);
    if (expected === undefined) {
      diags.push(
        diag(ErrorCodes.UnlistedFile, path, `payload file ${JSON.stringify(path)} is not listed in manifest files`),
      );
      continue;
    }
    if (seen.has(path)) {
      diags.push(diag(ErrorCodes.DuplicateFile, path, `payload file ${JSON.stringify(path)} appears more than once`));
      continue;
    }
    seen.add(path);
    const got = sha256Hex(entry.data);
    if (got !== expected) {
      diags.push(
        diag(
          ErrorCodes.DigestMismatch,
          path,
          `payload file ${JSON.stringify(path)} digest ${got} does not match manifest digest ${expected}`,
        ),
      );
    }
  }

  for (const file of manifest.files) {
    if (!seen.has(file.path)) {
      diags.push(
        diag(ErrorCodes.MissingFile, file.path, `manifest file ${JSON.stringify(file.path)} is missing from payload`),
      );
    }
  }
  return diags;
}
