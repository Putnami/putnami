/**
 * A strict JSON reader for wire documents that must be read the way the Go
 * protocol packages read them.
 *
 * `JSON.parse` is lossy in exactly the ways a wire contract cares about. It
 * silently keeps the last of a set of duplicate object keys, and it collapses
 * every numeric spelling to one double — so `1.0`, `1e0`, and `1` all arrive as
 * the JavaScript number `1`. A protocol that pins "the exact integer token 1"
 * cannot be enforced after that conversion has happened, because the
 * distinguishing information no longer exists.
 *
 * This reader keeps the raw source text of every value it parses, so a caller
 * can assert on the token that was actually written, and it rejects duplicate
 * keys and explicit nulls at any depth. Trailing content after the document is
 * rejected too. It mirrors `protocols/support/strict.go`
 * (`inspectJSONStructure` + `requireExactProtocolVersion` + `requireEOF`) and is
 * checked against that package's shared fixture corpus.
 *
 * It is a build-time reader for small documents; clarity beats speed here.
 */

export class StrictJsonError extends Error {}

interface ParsedValue {
  value: unknown;
  /** Exact source text of this value, whitespace-trimmed. */
  raw: string;
}

/** A parsed object member: its value plus the raw token that produced it. */
export type RawMembers = Map<string, ParsedValue>;

const WHITESPACE = new Set([' ', '\t', '\n', '\r']);

class Reader {
  private at = 0;
  /** Raw token text per member of the ROOT object, filled during the parse. */
  readonly topLevelMembers: RawMembers = new Map();

  constructor(private readonly source: string) {}

  private fail(message: string): never {
    throw new StrictJsonError(`${message} at offset ${this.at}`);
  }

  private skipWhitespace(): void {
    while (this.at < this.source.length && WHITESPACE.has(this.source[this.at] as string)) this.at += 1;
  }

  private peek(): string {
    if (this.at >= this.source.length) this.fail('unexpected end of input');
    return this.source[this.at] as string;
  }

  private expect(char: string): void {
    if (this.peek() !== char) this.fail(`expected ${JSON.stringify(char)}`);
    this.at += 1;
  }

  /** Parse one value and return it with the exact source slice it came from. */
  parseValue(path: string): ParsedValue {
    this.skipWhitespace();
    const start = this.at;
    const value = this.parseValueBody(path);
    return { value, raw: this.source.slice(start, this.at).trim() };
  }

  private parseValueBody(path: string): unknown {
    const char = this.peek();
    if (char === '{') return this.parseObject(path);
    if (char === '[') return this.parseArray(path);
    if (char === '"') return this.parseString();
    if (char === 't') return this.parseLiteral('true', true);
    if (char === 'f') return this.parseLiteral('false', false);
    if (char === 'n') {
      // Explicit null is rejected rather than parsed: in these contracts an
      // absent field makes no claim, while a null claims something unreadable.
      this.parseLiteral('null', null);
      throw new StrictJsonError(`explicit null is not allowed at ${path || 'the document root'}`);
    }
    return this.parseNumber();
  }

  private parseLiteral<T>(text: string, value: T): T {
    if (this.source.slice(this.at, this.at + text.length) !== text) this.fail(`invalid literal`);
    this.at += text.length;
    return value;
  }

  private parseNumber(): number {
    const start = this.at;
    if (this.peek() === '-') this.at += 1;
    while (this.at < this.source.length && /[0-9eE+.-]/.test(this.source[this.at] as string)) this.at += 1;
    const text = this.source.slice(start, this.at);
    // Reject what JSON does not allow but Number() would accept (e.g. "", "-",
    // "01", ".5", "1."): the grammar, not the coercion, decides.
    if (!/^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$/.test(text)) {
      this.at = start;
      this.fail(`invalid number ${JSON.stringify(text)}`);
    }
    return Number(text);
  }

  private parseString(): string {
    this.expect('"');
    let out = '';
    for (;;) {
      if (this.at >= this.source.length) this.fail('unterminated string');
      const char = this.source[this.at] as string;
      if (char === '"') {
        this.at += 1;
        return out;
      }
      if (char === '\\') {
        this.at += 1;
        out += this.parseEscape();
        continue;
      }
      if (char < ' ') this.fail('unescaped control character in string');
      out += char;
      this.at += 1;
    }
  }

  private parseEscape(): string {
    const char = this.peek();
    this.at += 1;
    switch (char) {
      case '"':
        return '"';
      case '\\':
        return '\\';
      case '/':
        return '/';
      case 'b':
        return '\b';
      case 'f':
        return '\f';
      case 'n':
        return '\n';
      case 'r':
        return '\r';
      case 't':
        return '\t';
      case 'u': {
        const hex = this.source.slice(this.at, this.at + 4);
        if (!/^[0-9a-fA-F]{4}$/.test(hex)) this.fail('invalid unicode escape');
        this.at += 4;
        return String.fromCharCode(Number.parseInt(hex, 16));
      }
      default:
        this.at -= 1;
        return this.fail('invalid escape sequence');
    }
  }

  private parseArray(path: string): unknown[] {
    this.expect('[');
    const items: unknown[] = [];
    this.skipWhitespace();
    if (this.peek() === ']') {
      this.at += 1;
      return items;
    }
    for (;;) {
      const item = this.parseValue(`${path}[${items.length}]`);
      items.push(item.value);
      this.skipWhitespace();
      const char = this.peek();
      if (char === ',') {
        this.at += 1;
        continue;
      }
      if (char === ']') {
        this.at += 1;
        return items;
      }
      this.fail('expected "," or "]"');
    }
  }

  private parseObject(path: string): Record<string, unknown> {
    this.expect('{');
    const out: Record<string, unknown> = {};
    const seen = new Set<string>();
    this.skipWhitespace();
    if (this.peek() === '}') {
      this.at += 1;
      return out;
    }
    for (;;) {
      this.skipWhitespace();
      const key = this.parseString();
      const field = path ? `${path}.${key}` : key;
      // Duplicate keys are the failure JSON.parse hides completely: it keeps
      // the last one, so a document can say two different things and read as
      // one of them.
      if (seen.has(key)) throw new StrictJsonError(`field ${JSON.stringify(field)} appears more than once`);
      seen.add(key);
      this.skipWhitespace();
      this.expect(':');
      const member = this.parseValue(field);
      out[key] = member.value;
      // `path === ''` is the root object, whose raw member tokens a caller
      // needs to assert on an exact spelling such as the integer token `1`.
      if (path === '') this.topLevelMembers.set(key, member);
      this.skipWhitespace();
      const char = this.peek();
      if (char === ',') {
        this.at += 1;
        continue;
      }
      if (char === '}') {
        this.at += 1;
        return out;
      }
      this.fail('expected "," or "}"');
    }
  }

  /** Reject anything after the document, mirroring the Go `requireEOF`. */
  requireEof(): void {
    this.skipWhitespace();
    if (this.at < this.source.length) {
      throw new StrictJsonError(`trailing JSON value is not allowed at offset ${this.at}`);
    }
  }
}

export interface StrictObject {
  /** The decoded object. */
  value: Record<string, unknown>;
  /** Raw source text per top-level member, for exact-token assertions. */
  members: RawMembers;
}

/**
 * Read a JSON object strictly: no duplicate keys at any depth, no explicit
 * nulls, no trailing content, and the raw token text of every top-level member
 * preserved.
 */
export function parseStrictJsonObject(source: string): StrictObject {
  const reader = new Reader(source);
  const root = reader.parseValue('');
  reader.requireEof();
  if (typeof root.value !== 'object' || root.value === null || Array.isArray(root.value)) {
    throw new StrictJsonError('document must be a JSON object');
  }
  return { value: root.value as Record<string, unknown>, members: reader.topLevelMembers };
}
