/**
 * Minimal in-memory tar (ustar/pax) reader for site-content bundle payloads.
 *
 * Scope is intentionally tight: bundles are small documentation payloads built
 * by Go's `archive/tar` writer (USTAR headers, PAX extension records only when
 * a value does not fit). Everything is parsed from a fully materialized,
 * digest-verified byte buffer — no streaming, no filesystem side effects. The
 * verifier (`verify.ts`) decides which entry types are acceptable; this reader
 * just surfaces them faithfully, so hostile inputs (traversal names, links,
 * devices) are reported rather than silently dropped.
 *
 * Hand-rolled on purpose: the repository rule is no new external dependencies.
 */

export interface TarEntry {
  /** Entry path exactly as recorded (prefix-joined, PAX `path` applied). */
  name: string;
  /** Raw tar typeflag byte as a single-char string ('0' for regular files). */
  typeflag: string;
  /** Link target for link entries, empty otherwise. */
  linkname: string;
  /** File content (empty for non-regular entries). */
  data: Uint8Array;
}

const BLOCK = 512;

class TarFormatError extends Error {}

function ascii(bytes: Uint8Array, start: number, length: number): string {
  let end = start;
  const max = start + length;
  while (end < max && bytes[end] !== 0) end++;
  return new TextDecoder().decode(bytes.subarray(start, end));
}

function parseOctal(bytes: Uint8Array, start: number, length: number): number {
  if ((bytes[start] & 0x80) !== 0) {
    // GNU base-256 numeric encoding — never emitted for the small payloads
    // this contract carries; reject instead of mis-parsing.
    throw new TarFormatError('unsupported base-256 numeric field in tar header');
  }
  const text = ascii(bytes, start, length).trim();
  if (text === '') return 0;
  const value = Number.parseInt(text, 8);
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new TarFormatError(`invalid octal field ${JSON.stringify(text)} in tar header`);
  }
  return value;
}

function isZeroBlock(bytes: Uint8Array, offset: number): boolean {
  for (let i = offset; i < offset + BLOCK; i++) {
    if (bytes[i] !== 0) return false;
  }
  return true;
}

/** Parse PAX extended header records ("<len> <key>=<value>\n"). */
function parsePaxRecords(data: Uint8Array): Map<string, string> {
  const records = new Map<string, string>();
  const decoder = new TextDecoder();
  let offset = 0;
  while (offset < data.length) {
    let spaceAt = offset;
    while (spaceAt < data.length && data[spaceAt] !== 0x20) spaceAt++;
    const len = Number.parseInt(decoder.decode(data.subarray(offset, spaceAt)), 10);
    if (!Number.isSafeInteger(len) || len <= 0 || offset + len > data.length) {
      throw new TarFormatError('invalid PAX record length');
    }
    const record = decoder.decode(data.subarray(spaceAt + 1, offset + len - 1));
    const eq = record.indexOf('=');
    if (eq < 0) throw new TarFormatError('invalid PAX record (missing "=")');
    records.set(record.slice(0, eq), record.slice(eq + 1));
    offset += len;
  }
  return records;
}

/**
 * Read every entry of an uncompressed tar buffer. Throws `Error` on a
 * structurally invalid archive (truncated, bad numeric fields, unsupported
 * global PAX headers) — the caller maps that to a parse-error diagnostic.
 */
export function readTarEntries(tarBytes: Uint8Array): TarEntry[] {
  const entries: TarEntry[] = [];
  let offset = 0;
  let pendingPax: Map<string, string> | null = null;

  while (offset + BLOCK <= tarBytes.length) {
    if (isZeroBlock(tarBytes, offset)) break; // end-of-archive marker

    const header = tarBytes.subarray(offset, offset + BLOCK);
    const size = parseOctal(header, 124, 12);
    const typeflagByte = header[156];
    const typeflag = typeflagByte === 0 ? '0' : String.fromCharCode(typeflagByte);
    const dataStart = offset + BLOCK;
    const dataEnd = dataStart + size;
    if (dataEnd > tarBytes.length) throw new TarFormatError('truncated tar entry');
    const data = tarBytes.subarray(dataStart, dataEnd);
    offset = dataStart + Math.ceil(size / BLOCK) * BLOCK;

    if (typeflag === 'x') {
      // PAX extended header for the NEXT entry (Go's writer emits these when a
      // path does not fit the USTAR fields).
      pendingPax = parsePaxRecords(data);
      continue;
    }
    if (typeflag === 'g') {
      throw new TarFormatError('unsupported global PAX header in payload');
    }

    let name = ascii(header, 0, 100);
    const prefix = ascii(header, 345, 155);
    if (prefix !== '') name = `${prefix}/${name}`;
    let linkname = ascii(header, 157, 100);
    if (pendingPax) {
      const paxPath = pendingPax.get('path');
      if (paxPath !== undefined) name = paxPath;
      const paxLink = pendingPax.get('linkpath');
      if (paxLink !== undefined) linkname = paxLink;
      pendingPax = null;
    }

    entries.push({ name, typeflag, linkname, data: Uint8Array.from(data) });
  }
  return entries;
}

// ---------------------------------------------------------------------------
// Writer — used by the local test/fixture doubles (kept beside the reader so
// the two stay in sync; production only ever reads).
// ---------------------------------------------------------------------------

export interface TarWriteEntry {
  name: string;
  typeflag?: string;
  linkname?: string;
  data?: Uint8Array | string;
}

function writeOctal(block: Uint8Array, start: number, length: number, value: number): void {
  const text = value.toString(8).padStart(length - 1, '0');
  for (let i = 0; i < text.length; i++) block[start + i] = text.charCodeAt(i);
  block[start + length - 1] = 0;
}

function writeAscii(block: Uint8Array, start: number, text: string): void {
  for (let i = 0; i < text.length; i++) block[start + i] = text.charCodeAt(i);
}

/** Build an uncompressed USTAR archive (test double for a producer payload). */
export function writeTarEntries(entries: TarWriteEntry[]): Uint8Array {
  const chunks: Uint8Array[] = [];
  for (const entry of entries) {
    const data =
      typeof entry.data === 'string' ? new TextEncoder().encode(entry.data) : (entry.data ?? new Uint8Array(0));
    if (entry.name.length > 100) throw new Error(`test tar writer: name too long: ${entry.name}`);
    const header = new Uint8Array(BLOCK);
    writeAscii(header, 0, entry.name);
    writeOctal(header, 100, 8, 0o644); // mode
    writeOctal(header, 108, 8, 0); // uid
    writeOctal(header, 116, 8, 0); // gid
    writeOctal(header, 124, 12, data.length);
    writeOctal(header, 136, 12, 0); // mtime — fixed for determinism
    header[156] = (entry.typeflag ?? '0').charCodeAt(0);
    if (entry.linkname) writeAscii(header, 157, entry.linkname);
    writeAscii(header, 257, 'ustar');
    header[263] = 0x30; // version "00"
    header[264] = 0x30;
    // checksum: computed with the checksum field treated as spaces
    for (let i = 148; i < 156; i++) header[i] = 0x20;
    let sum = 0;
    for (const byte of header) sum += byte;
    writeOctal(header, 148, 7, sum);
    header[155] = 0x20;
    chunks.push(header, data);
    const pad = (BLOCK - (data.length % BLOCK)) % BLOCK;
    if (pad > 0) chunks.push(new Uint8Array(pad));
  }
  chunks.push(new Uint8Array(BLOCK * 2)); // end-of-archive
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const chunk of chunks) {
    out.set(chunk, at);
    at += chunk.length;
  }
  return out;
}
