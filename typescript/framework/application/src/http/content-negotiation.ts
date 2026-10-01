import { NotAcceptableException } from '@putnami/runtime';
import { escapeHtml } from '@putnami/utils';
import { HttpResponse } from './http-response';

interface MediaType {
  type: string;
  quality: number;
}

/**
 * Parse an Accept header into an ordered list of media types.
 * Entries are sorted by quality (descending), then by specificity.
 */
export function parseAccept(header: string | null): MediaType[] {
  if (!header) {
    return [{ type: '*/*', quality: 1 }];
  }

  const types: MediaType[] = [];
  for (const part of header.split(',')) {
    const trimmed = part.trim();
    if (!trimmed) continue;

    const [mediaType, ...params] = trimmed.split(';').map((s) => s.trim());
    let quality = 1;
    for (const param of params) {
      const match = param.match(/^q\s*=\s*([0-9.]+)$/);
      if (match) {
        quality = Number.parseFloat(match[1]);
      }
    }
    types.push({ type: mediaType, quality });
  }

  types.sort((a, b) => {
    if (b.quality !== a.quality) return b.quality - a.quality;
    // More specific types first (* /* last)
    const aWild = a.type.includes('*') ? 1 : 0;
    const bWild = b.type.includes('*') ? 1 : 0;
    return aWild - bWild;
  });

  return types;
}

function mediaTypeMatches(pattern: string, candidate: string): boolean {
  if (pattern === '*/*' || candidate === '*/*') return true;
  if (pattern === candidate) return true;
  const [pType] = pattern.split('/');
  const [cType] = candidate.split('/');
  if (pType === '*' || cType === '*') return true;
  return pType === cType && (pattern.endsWith('/*') || candidate.endsWith('/*'));
}

/**
 * Find the best matching content type from the accepted types and the server's offered types.
 * Returns the first match or undefined if no match.
 */
export function negotiateType(accepted: MediaType[], offered: string[]): string | undefined {
  for (const accept of accepted) {
    for (const offer of offered) {
      if (mediaTypeMatches(accept.type, offer)) {
        return offer;
      }
    }
  }
  return undefined;
}

/** Server-offered media types, ordered by preference. Hoisted so it isn't reallocated per response. */
const OFFERED_TYPES = [
  'application/json',
  'text/plain',
  'text/html',
  'application/xml',
  'text/xml',
  'application/yaml',
  'text/yaml',
] as const;

/**
 * Serialise a handler result based on the negotiated content type.
 *
 * Supported serialisation targets:
 * - `application/json` → JSON.stringify
 * - `text/plain` → YAML as plain text (human-readable)
 * - `text/html` → wraps in minimal HTML (useful for debugging)
 * - `application/xml` / `text/xml` → simple XML serialisation
 * - `application/yaml` / `text/yaml` → YAML serialisation
 * - `* /*` / unmatched → falls back to JSON
 */
export function negotiateResponse(data: unknown, acceptHeader: string | null): HttpResponse {
  // Fast path: the overwhelming majority of API responses are JSON and the
  // client either omits Accept or sends `*/*` / `application/json`. For those
  // exact inputs the full negotiation below provably resolves to JSON, so we
  // skip parsing + sorting + the offered-type scan entirely.
  if (acceptHeader === null) {
    return serializeJson(data);
  }
  const trimmed = acceptHeader.trim();
  if (trimmed === '*/*' || trimmed === 'application/json') {
    return serializeJson(data);
  }

  const accepted = parseAccept(acceptHeader);

  const chosen = negotiateType(accepted, OFFERED_TYPES as unknown as string[]);

  if (!chosen) {
    throw new NotAcceptableException(
      `None of the requested types are supported. Supported: ${OFFERED_TYPES.join(', ')}`,
    );
  }

  switch (chosen) {
    case 'text/plain':
      return serializeYaml(data, 'text/plain');
    case 'text/html':
      return serializeHtml(data);
    case 'application/xml':
    case 'text/xml':
      return serializeXml(data, chosen);
    case 'application/yaml':
    case 'text/yaml':
      return serializeYaml(data, chosen);
    default:
      return serializeJson(data);
  }
}

function serializeJson(data: unknown): HttpResponse {
  return HttpResponse.json(data);
}

function serializeHtml(data: unknown): HttpResponse {
  const json = JSON.stringify(data, null, 2);
  const html = `<!DOCTYPE html><html><head><meta charset="utf-8"></head><body><pre>${escapeHtml(json)}</pre></body></html>`;
  return new HttpResponse(html, {
    status: 200,
    headers: { 'Content-Type': 'text/html; charset=utf-8' },
  });
}

function serializeXml(data: unknown, contentType: string): HttpResponse {
  const xml = `<?xml version="1.0" encoding="UTF-8"?>\n${toXml(data, 'root')}`;
  return new HttpResponse(xml, {
    status: 200,
    headers: { 'Content-Type': `${contentType}; charset=utf-8` },
  });
}

function serializeYaml(data: unknown, contentType: string): HttpResponse {
  const yaml = toYaml(data, 0);
  return new HttpResponse(yaml, {
    status: 200,
    headers: { 'Content-Type': `${contentType}; charset=utf-8` },
  });
}

function toYaml(value: unknown, indent: number): string {
  const pad = '  '.repeat(indent);

  if (value === null || value === undefined) {
    return 'null';
  }
  if (typeof value === 'boolean') {
    return String(value);
  }
  if (typeof value === 'number') {
    return String(value);
  }
  if (typeof value === 'string') {
    // Quote strings that could be misinterpreted or contain special chars
    if (
      value === '' ||
      value === 'true' ||
      value === 'false' ||
      value === 'null' ||
      /^[0-9]/.test(value) ||
      /[:#{}[\],&*?|>!'"%@`]/.test(value) ||
      value.includes('\n')
    ) {
      return JSON.stringify(value);
    }
    return value;
  }
  if (Array.isArray(value)) {
    if (value.length === 0) return '[]';
    return value
      .map((item) => {
        const serialized = toYaml(item, indent + 1);
        const isComplex = typeof item === 'object' && item !== null;
        if (isComplex) {
          // For objects/arrays, put first key on same line as dash
          return `${pad}- ${serialized.trimStart()}`;
        }
        return `${pad}- ${serialized}`;
      })
      .join('\n');
  }
  if (typeof value === 'object') {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return '{}';
    return entries
      .map(([k, v]) => {
        const serializedValue = toYaml(v, indent + 1);
        const isMultiline = serializedValue.includes('\n');
        if (isMultiline) {
          return `${pad}${k}:\n${serializedValue}`;
        }
        return `${pad}${k}: ${serializedValue}`;
      })
      .join('\n');
  }
  return String(value);
}

// XML element names must be NCNames: start with a letter or `_`, then letters,
// digits, `-`, `.` or `_`. Object keys are arbitrary (and may be user-derived),
// so coerce them into a safe tag name to avoid emitting malformed/injectable XML.
const XML_NAME_INVALID_RE = /[^A-Za-z0-9._-]/g;
const XML_NAME_INVALID_START_RE = /^[^A-Za-z_]/;

function toXmlTagName(key: string): string {
  let name = key.replace(XML_NAME_INVALID_RE, '_');
  if (name === '' || XML_NAME_INVALID_START_RE.test(name)) {
    name = `_${name}`;
  }
  // `xml` (any case) is a reserved prefix; defuse it so the name stays valid.
  if (/^xml/i.test(name)) {
    name = `_${name}`;
  }
  return name;
}

function toXml(value: unknown, tagName: string): string {
  if (value === null || value === undefined) {
    return `<${tagName}/>`;
  }
  if (Array.isArray(value)) {
    return value.map((item) => toXml(item, 'item')).join('');
  }
  if (typeof value === 'object') {
    const inner = Object.entries(value as Record<string, unknown>)
      .map(([k, v]) => toXml(v, toXmlTagName(k)))
      .join('');
    return `<${tagName}>${inner}</${tagName}>`;
  }
  return `<${tagName}>${escapeXml(String(value))}</${tagName}>`;
}

const XML_ENTITIES: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&apos;' };
const XML_ESCAPE_RE = /[&<>"']/g;

function escapeXml(s: string): string {
  return s.replace(XML_ESCAPE_RE, (c) => XML_ENTITIES[c]);
}
