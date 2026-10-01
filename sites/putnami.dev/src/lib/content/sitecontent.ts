/**
 * TypeScript reimplementation of the `site-content-bundle/v1` contract rules
 * (`protocols/sitecontent`, Go). There is no TypeScript protocol mirror yet, so
 * this module ports the exact validation semantics the Go reference helpers
 * enforce — format-version envelope gating, strict parsing of readable
 * manifests (unknown fields rejected), path/mount/digest shape rules, and
 * URL-prefix overlap — keeping the diagnostic codes byte-identical to the Go
 * taxonomy so producer and consumer speak the same error language.
 *
 * Any semantic change here must be checked against `protocols/sitecontent`
 * (`strict.go`, `payload.go`, `merge.go` and the shared fixture corpus).
 */

/** Bundle format version a consumer built against this module implements. */
export const FORMAT_VERSION = '1.0';
/** Major of {@link FORMAT_VERSION}: bundles with a newer major are ignored. */
export const FORMAT_MAJOR = 1;

/** Canonical sitecontent diagnostic codes — mirror `strict.go` exactly. */
export const ErrorCodes = {
  ParseError: 'sitecontent.parse_error',
  UnknownField: 'sitecontent.unknown_field',
  InvalidFormatVersion: 'sitecontent.invalid_format_version',
  InvalidName: 'sitecontent.invalid_name',
  MissingMounts: 'sitecontent.missing_mounts',
  InvalidMount: 'sitecontent.invalid_mount',
  InvalidSource: 'sitecontent.invalid_source',
  InvalidPath: 'sitecontent.invalid_path',
  InvalidDigest: 'sitecontent.invalid_digest',
  MissingFiles: 'sitecontent.missing_files',
  DuplicateFile: 'sitecontent.duplicate_file',
  PathOutsideMount: 'sitecontent.path_outside_mount',
  InvalidPayloadEntry: 'sitecontent.invalid_payload_entry',
  InvalidSymlink: 'sitecontent.invalid_symlink',
  UnlistedFile: 'sitecontent.unlisted_file',
  MissingFile: 'sitecontent.missing_file',
  DigestMismatch: 'sitecontent.digest_mismatch',
  MountCollision: 'sitecontent.mount_collision',
} as const;

export interface Diagnostic {
  code: string;
  field: string;
  message: string;
}

export interface Mount {
  urlPrefix: string;
}

export interface SourceRef {
  repo: string;
  commit: string;
}

export interface FileEntry {
  path: string;
  digest: string;
}

/** `bundle.json`: the manifest that travels alongside a bundle's payload. */
export interface Manifest {
  $schema?: string;
  formatVersion: string;
  name: string;
  mounts: Mount[];
  source: SourceRef;
  files: FileEntry[];
}

const BUNDLE_NAME_PATTERN = /^[a-z0-9][a-z0-9._-]{0,63}$/;
const MOUNT_SEGMENT_PATTERN = /^[a-z0-9][a-z0-9._-]*$/;
const FORMAT_VERSION_PATTERN = /^(\d+)(?:\.(\d+))?$/;

function diag(code: string, field: string, message: string): Diagnostic {
  return { code, field, message };
}

/** Parse the major of a "major[.minor]" version string; null when malformed. */
export function formatVersionMajor(v: string): number | null {
  const m = FORMAT_VERSION_PATTERN.exec(v);
  if (!m) return null;
  const major = Number.parseInt(m[1], 10);
  return Number.isSafeInteger(major) ? major : null;
}

/**
 * Whether a consumer built against {@link FORMAT_MAJOR} may read a bundle
 * stamped `formatVersion` v. A newer major MUST be ignored (fall back to baked
 * content); a malformed version is never compatible.
 */
export function compatibleFormatVersion(v: string): boolean {
  return formatVersionMajor(v) === FORMAT_MAJOR;
}

/** sha256 bare-hex: exactly 64 lowercase hex chars, no "sha256:" prefix. */
export function validDigest(s: string): boolean {
  return /^[0-9a-f]{64}$/.test(s);
}

/**
 * Safe payload-relative path: non-empty, forward-slash separated, no leading
 * "/", no backslash, no NUL, and no empty / "." / ".." segment.
 */
export function validRelPath(p: string): boolean {
  if (p === '' || p.startsWith('/') || p.includes('\\') || p.includes('\0')) return false;
  return p.split('/').every((seg) => seg !== '' && seg !== '.' && seg !== '..');
}

/**
 * Well-formed mount URL prefix: absolute, not the root "/", no trailing slash,
 * every segment a lowercase URL-safe token (which excludes "", "." and "..").
 */
export function validMountPrefix(p: string): boolean {
  if (!p.startsWith('/') || p === '/' || p.endsWith('/')) return false;
  return p
    .slice(1)
    .split('/')
    .every((seg) => MOUNT_SEGMENT_PATTERN.test(seg));
}

/** The site URL a payload file at `p` is served at. */
export function urlPath(p: string): string {
  return `/${p}`;
}

/**
 * Whether `url` is equal to `prefix` or below it on a path-segment boundary:
 * "/docs/cloud" is inside "/docs"; "/docs-cloud" is not.
 */
export function urlInPrefix(url: string, prefix: string): boolean {
  if (prefix === '' || url === '') return false;
  if (prefix === '/') return url.startsWith('/');
  return url === prefix || url.startsWith(`${prefix}/`);
}

/** Equal and parent/child prefixes conflict; siblings do not. */
export function prefixesConflict(a: string, b: string): boolean {
  return urlInPrefix(a, b) || urlInPrefix(b, a);
}

/** The declared mount owning a payload-relative file path, or null. */
export function mountForPath(mounts: Mount[], path: string): Mount | null {
  if (!validRelPath(path)) return null;
  const url = urlPath(path);
  return mounts.find((m) => urlInPrefix(url, m.urlPrefix)) ?? null;
}

export function hasErrors(diags: Diagnostic[]): boolean {
  return diags.length > 0;
}

// ---------------------------------------------------------------------------
// Strict manifest parsing (unknown fields rejected, exactly one JSON object)
// ---------------------------------------------------------------------------

const MANIFEST_KEYS = new Set(['$schema', 'formatVersion', 'name', 'mounts', 'source', 'files']);
const MOUNT_KEYS = new Set(['urlPrefix']);
const SOURCE_KEYS = new Set(['repo', 'commit']);
const FILE_KEYS = new Set(['path', 'digest']);

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * Read only the stable version envelope before decoding a versioned manifest
 * shape. Unknown fields are intentionally ignored here so a newer major can be
 * skipped; invalid JSON, non-objects, and non-string/missing versions return
 * null and therefore never trigger fallback.
 */
export function manifestFormatVersion(data: string | Uint8Array): string | null {
  const text = typeof data === 'string' ? data : new TextDecoder().decode(data);
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch {
    return null;
  }
  if (!isRecord(doc) || typeof doc['formatVersion'] !== 'string') return null;
  return doc['formatVersion'];
}

function unknownFieldDiags(obj: Record<string, unknown>, allowed: Set<string>, at: string): Diagnostic[] {
  return Object.keys(obj)
    .filter((k) => !allowed.has(k))
    .map((k) =>
      diag(ErrorCodes.UnknownField, at ? `${at}.${k}` : k, `unknown field ${JSON.stringify(k)} in bundle manifest`),
    );
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/**
 * Strict-decode `bundle.json`: invalid JSON, a non-object document, wrong field
 * types, and unknown fields all fail (mirrors Go's DisallowUnknownFields).
 * Returns a manifest only when decoding produced no errors.
 */
export function parseManifest(data: string | Uint8Array): { manifest: Manifest | null; diagnostics: Diagnostic[] } {
  const text = typeof data === 'string' ? data : new TextDecoder().decode(data);
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch (error) {
    return { manifest: null, diagnostics: [diag(ErrorCodes.ParseError, '', (error as Error).message)] };
  }
  if (!isRecord(doc)) {
    return { manifest: null, diagnostics: [diag(ErrorCodes.ParseError, '', 'manifest must be a JSON object')] };
  }

  const diags: Diagnostic[] = unknownFieldDiags(doc, MANIFEST_KEYS, '');

  const mounts: Mount[] = [];
  if (doc['mounts'] !== undefined) {
    if (!Array.isArray(doc['mounts'])) {
      diags.push(diag(ErrorCodes.ParseError, 'mounts', 'mounts must be an array'));
    } else {
      doc['mounts'].forEach((raw, i) => {
        if (!isRecord(raw)) {
          diags.push(diag(ErrorCodes.ParseError, `mounts[${i}]`, 'mount must be an object'));
          return;
        }
        diags.push(...unknownFieldDiags(raw, MOUNT_KEYS, `mounts[${i}]`));
        mounts.push({ urlPrefix: str(raw['urlPrefix']) });
      });
    }
  }

  const files: FileEntry[] = [];
  if (doc['files'] !== undefined) {
    if (!Array.isArray(doc['files'])) {
      diags.push(diag(ErrorCodes.ParseError, 'files', 'files must be an array'));
    } else {
      doc['files'].forEach((raw, i) => {
        if (!isRecord(raw)) {
          diags.push(diag(ErrorCodes.ParseError, `files[${i}]`, 'file must be an object'));
          return;
        }
        diags.push(...unknownFieldDiags(raw, FILE_KEYS, `files[${i}]`));
        files.push({ path: str(raw['path']), digest: str(raw['digest']) });
      });
    }
  }

  let source: SourceRef = { repo: '', commit: '' };
  if (doc['source'] !== undefined) {
    if (!isRecord(doc['source'])) {
      diags.push(diag(ErrorCodes.ParseError, 'source', 'source must be an object'));
    } else {
      diags.push(...unknownFieldDiags(doc['source'], SOURCE_KEYS, 'source'));
      source = { repo: str(doc['source']['repo']), commit: str(doc['source']['commit']) };
    }
  }

  if (hasErrors(diags)) return { manifest: null, diagnostics: diags };
  return {
    manifest: {
      ...(typeof doc['$schema'] === 'string' ? { $schema: doc['$schema'] } : {}),
      formatVersion: str(doc['formatVersion']),
      name: str(doc['name']),
      mounts,
      source,
      files,
    },
    diagnostics: [],
  };
}

// ---------------------------------------------------------------------------
// Manifest validation (mirrors strict.go ValidateManifest)
// ---------------------------------------------------------------------------

function validateName(name: string): Diagnostic[] {
  if (name === '') return [diag(ErrorCodes.InvalidName, 'name', 'name is required')];
  if (!BUNDLE_NAME_PATTERN.test(name)) {
    return [
      diag(
        ErrorCodes.InvalidName,
        'name',
        `name ${JSON.stringify(name)} does not match canonical pattern ${BUNDLE_NAME_PATTERN.source}`,
      ),
    ];
  }
  return [];
}

function validateMounts(mounts: Mount[]): Diagnostic[] {
  if (mounts.length === 0) {
    return [diag(ErrorCodes.MissingMounts, 'mounts', 'a bundle must declare at least one mount')];
  }
  const diags: Diagnostic[] = [];
  mounts.forEach((mount, i) => {
    if (!validMountPrefix(mount.urlPrefix)) {
      diags.push(
        diag(
          ErrorCodes.InvalidMount,
          `mounts[${i}].urlPrefix`,
          `urlPrefix ${JSON.stringify(mount.urlPrefix)} must be an absolute, normalized, lowercase site path ` +
            `with no '..' segment, no trailing slash, and not the root '/'`,
        ),
      );
    }
  });
  for (let i = 0; i < mounts.length; i++) {
    for (let j = i + 1; j < mounts.length; j++) {
      if (prefixesConflict(mounts[i].urlPrefix, mounts[j].urlPrefix)) {
        diags.push(
          diag(
            ErrorCodes.MountCollision,
            `mounts[${j}].urlPrefix`,
            `mount ${JSON.stringify(mounts[j].urlPrefix)} overlaps mount ` +
              `${JSON.stringify(mounts[i].urlPrefix)}; a bundle's mounts may not overlap`,
          ),
        );
      }
    }
  }
  return diags;
}

function validateSource(source: SourceRef): Diagnostic[] {
  const diags: Diagnostic[] = [];
  if (source.repo.trim() === '') {
    diags.push(diag(ErrorCodes.InvalidSource, 'source.repo', 'source.repo is required'));
  }
  if (source.commit === '' || /\s/.test(source.commit)) {
    diags.push(
      diag(ErrorCodes.InvalidSource, 'source.commit', 'source.commit is required and must not contain whitespace'),
    );
  }
  return diags;
}

function validateFiles(manifest: Manifest): Diagnostic[] {
  if (manifest.files.length === 0) {
    return [diag(ErrorCodes.MissingFiles, 'files', 'a bundle must list at least one file')];
  }
  const diags: Diagnostic[] = [];
  const seen = new Map<string, number>();
  manifest.files.forEach((file, i) => {
    const field = `files[${i}]`;
    if (!validRelPath(file.path)) {
      diags.push(
        diag(
          ErrorCodes.InvalidPath,
          `${field}.path`,
          `path ${JSON.stringify(file.path)} must be a forward-slash relative path with no leading '/', ` +
            `'.' or '..' segment, or backslash`,
        ),
      );
    } else if (!mountForPath(manifest.mounts, file.path)) {
      diags.push(
        diag(
          ErrorCodes.PathOutsideMount,
          `${field}.path`,
          `path ${JSON.stringify(file.path)} (served at ${JSON.stringify(urlPath(file.path))}) ` +
            `falls under no declared mount`,
        ),
      );
    }
    if (!validDigest(file.digest)) {
      diags.push(
        diag(
          ErrorCodes.InvalidDigest,
          `${field}.digest`,
          `digest ${JSON.stringify(file.digest)} must be 64 lowercase hex chars (sha256 bare-hex)`,
        ),
      );
    }
    const first = seen.get(file.path);
    if (first !== undefined) {
      diags.push(
        diag(
          ErrorCodes.DuplicateFile,
          `files[${i}].path`,
          `path ${JSON.stringify(file.path)} duplicates files[${first}].path; every payload file must be listed once`,
        ),
      );
    } else {
      seen.set(file.path, i);
    }
  });
  return diags;
}

/**
 * Structural invariants of a parsed manifest: compatible format-version major,
 * canonical name, at least one valid non-overlapping mount, a complete source,
 * and per-file a safe relative path under a declared mount plus a well-formed
 * digest. Does NOT touch the payload — see `verify.ts`.
 */
export function validateManifest(manifest: Manifest): Diagnostic[] {
  const diags: Diagnostic[] = [];
  if (!compatibleFormatVersion(manifest.formatVersion)) {
    diags.push(
      diag(
        ErrorCodes.InvalidFormatVersion,
        'formatVersion',
        `formatVersion ${JSON.stringify(manifest.formatVersion)} is not readable by this parser ` +
          `(want major ${FORMAT_MAJOR})`,
      ),
    );
  }
  diags.push(...validateName(manifest.name));
  diags.push(...validateMounts(manifest.mounts));
  diags.push(...validateSource(manifest.source));
  diags.push(...validateFiles(manifest));
  return diags;
}
