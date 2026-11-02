/**
 * A bounded ICU MessageFormat implementation on the platform's `Intl`.
 *
 * Supported: plain text, `'` quoting, `{name}`, `{name, number[, integer|percent]}`,
 * `{name, date|time[, short|medium|long|full]}`, `{name, plural, ...}` and
 * `{name, selectordinal, ...}` with `=N` and `offset:N`, `#` inside plural
 * branches, and `{name, select, ...}`. Anything else is a syntax error, so a
 * catalog cannot carry a message the formatter would print wrongly.
 */

export type IcuArgumentKind = 'string' | 'number' | 'date' | 'plural' | 'select';

export type IcuNode =
  | { type: 'text'; value: string }
  | { type: 'pound' }
  | { type: 'argument'; name: string }
  | { type: 'number'; name: string; style?: 'integer' | 'percent' }
  | { type: 'date' | 'time'; name: string; style?: 'short' | 'medium' | 'long' | 'full' }
  | {
      type: 'plural' | 'selectordinal';
      name: string;
      offset: number;
      options: Record<string, IcuNode[]>;
    }
  | { type: 'select'; name: string; options: Record<string, IcuNode[]> };

export class IcuSyntaxError extends Error {
  constructor(
    message: string,
    readonly offset: number,
  ) {
    super(`${message} at offset ${offset}`);
    this.name = 'IcuSyntaxError';
  }
}

const IDENT = /[A-Za-z_][A-Za-z0-9_]*/y;
const NUMBER_STYLES = new Set(['integer', 'percent']);
const DATE_STYLES = new Set(['short', 'medium', 'long', 'full']);
const PLURAL_KEY = /(=\d+|zero|one|two|few|many|other)/y;

class Parser {
  private pos = 0;

  constructor(private readonly src: string) {}

  parse(): IcuNode[] {
    const nodes = this.message(false);
    if (this.pos < this.src.length) {
      throw new IcuSyntaxError(`Unexpected '${this.src[this.pos]}'`, this.pos);
    }
    return nodes;
  }

  private message(inPlural: boolean): IcuNode[] {
    const nodes: IcuNode[] = [];
    let text = '';
    const flush = () => {
      if (text) nodes.push({ type: 'text', value: text });
      text = '';
    };
    while (this.pos < this.src.length) {
      const ch = this.src[this.pos];
      if (ch === '}') break;
      if (ch === '{') {
        flush();
        nodes.push(this.argument());
        continue;
      }
      if (ch === '#' && inPlural) {
        flush();
        nodes.push({ type: 'pound' });
        this.pos++;
        continue;
      }
      if (ch === "'") {
        text += this.quoted(inPlural);
        continue;
      }
      text += ch;
      this.pos++;
    }
    flush();
    return nodes;
  }

  /** `''` is a literal quote; `'` before a syntax character opens a quoted run. */
  private quoted(inPlural: boolean): string {
    const next = this.src[this.pos + 1];
    if (next === "'") {
      this.pos += 2;
      return "'";
    }
    if (next === '{' || next === '}' || (inPlural && next === '#')) {
      const end = this.src.indexOf("'", this.pos + 1);
      if (end < 0) throw new IcuSyntaxError('Unterminated quote', this.pos);
      const value = this.src.slice(this.pos + 1, end);
      this.pos = end + 1;
      return value;
    }
    this.pos++;
    return "'";
  }

  private space() {
    while (this.pos < this.src.length && /\s/.test(this.src[this.pos] as string)) this.pos++;
  }

  private expect(ch: string) {
    if (this.src[this.pos] !== ch) {
      throw new IcuSyntaxError(`Expected '${ch}'`, this.pos);
    }
    this.pos++;
  }

  private match(re: RegExp): string | undefined {
    re.lastIndex = this.pos;
    const m = re.exec(this.src);
    if (!m) return undefined;
    this.pos += m[0].length;
    return m[0];
  }

  private ident(what: string): string {
    const name = this.match(IDENT);
    if (!name) throw new IcuSyntaxError(`Expected ${what}`, this.pos);
    return name;
  }

  private argument(): IcuNode {
    this.expect('{');
    this.space();
    const name = this.ident('an argument name');
    this.space();
    if (this.src[this.pos] === '}') {
      this.pos++;
      return { type: 'argument', name };
    }
    this.expect(',');
    this.space();
    const kindAt = this.pos;
    const kind = this.ident('an argument type');
    this.space();
    switch (kind) {
      case 'number':
      case 'date':
      case 'time': {
        let style: string | undefined;
        if (this.src[this.pos] === ',') {
          this.pos++;
          this.space();
          const styleAt = this.pos;
          style = this.ident('a style');
          const allowed = kind === 'number' ? NUMBER_STYLES : DATE_STYLES;
          if (!allowed.has(style)) throw new IcuSyntaxError(`Unsupported ${kind} style '${style}'`, styleAt);
          this.space();
        }
        this.expect('}');
        if (kind === 'number') {
          return { type: 'number', name, ...(style ? { style: style as 'integer' | 'percent' } : {}) };
        }
        return {
          type: kind,
          name,
          ...(style ? { style: style as 'short' | 'medium' | 'long' | 'full' } : {}),
        };
      }
      case 'plural':
      case 'selectordinal':
      case 'select': {
        this.expect(',');
        this.space();
        let offset = 0;
        if (kind !== 'select' && this.src.startsWith('offset:', this.pos)) {
          this.pos += 'offset:'.length;
          const digits = this.match(/\d+/y);
          if (!digits) throw new IcuSyntaxError('Expected an offset', this.pos);
          offset = Number(digits);
          this.space();
        }
        const options: Record<string, IcuNode[]> = {};
        while (this.src[this.pos] !== '}') {
          if (this.pos >= this.src.length) throw new IcuSyntaxError(`Unterminated ${kind}`, kindAt);
          const keyAt = this.pos;
          const key = kind === 'select' ? this.ident('a select key') : this.match(PLURAL_KEY);
          if (!key) throw new IcuSyntaxError(`Expected a ${kind} key`, keyAt);
          if (Object.hasOwn(options, key)) throw new IcuSyntaxError(`Duplicate key '${key}'`, keyAt);
          this.space();
          this.expect('{');
          options[key] = this.message(kind !== 'select');
          this.expect('}');
          this.space();
        }
        this.pos++;
        if (!options['other']) throw new IcuSyntaxError(`A ${kind} needs an 'other' branch`, kindAt);
        return kind === 'select' ? { type: 'select', name, options } : { type: kind, name, offset, options };
      }
      default:
        throw new IcuSyntaxError(`Unsupported argument type '${kind}'`, kindAt);
    }
  }
}

export function parseIcu(message: string): IcuNode[] {
  return new Parser(message).parse();
}

/** Every argument a message reads, with the kind of value it expects. */
export function icuArguments(
  nodes: IcuNode[],
  into = new Map<string, IcuArgumentKind>(),
): Map<string, IcuArgumentKind> {
  for (const node of nodes) {
    switch (node.type) {
      case 'argument':
        if (!into.has(node.name)) into.set(node.name, 'string');
        break;
      case 'number':
        into.set(node.name, 'number');
        break;
      case 'date':
      case 'time':
        into.set(node.name, 'date');
        break;
      case 'plural':
      case 'selectordinal':
        into.set(node.name, 'plural');
        for (const branch of Object.values(node.options)) icuArguments(branch, into);
        break;
      case 'select':
        into.set(node.name, 'select');
        for (const branch of Object.values(node.options)) icuArguments(branch, into);
        break;
    }
  }
  return into;
}

export type IcuValue = string | number | bigint | Date | boolean | null | undefined;
export type IcuValues = Record<string, IcuValue>;

const formatters = new Map<string, Intl.NumberFormat | Intl.DateTimeFormat | Intl.PluralRules>();

function cached<T extends Intl.NumberFormat | Intl.DateTimeFormat | Intl.PluralRules>(key: string, make: () => T): T {
  let formatter = formatters.get(key) as T | undefined;
  if (!formatter) {
    formatter = make();
    formatters.set(key, formatter);
  }
  return formatter;
}

function numberFormat(locale: string, style?: 'integer' | 'percent'): Intl.NumberFormat {
  return cached(
    `n:${locale}:${style ?? ''}`,
    () =>
      new Intl.NumberFormat(
        locale,
        style === 'percent' ? { style: 'percent' } : style === 'integer' ? { maximumFractionDigits: 0 } : undefined,
      ),
  );
}

function dateFormat(locale: string, type: 'date' | 'time', style = 'medium'): Intl.DateTimeFormat {
  const s = style as 'short' | 'medium' | 'long' | 'full';
  return cached(
    `d:${locale}:${type}:${s}`,
    () => new Intl.DateTimeFormat(locale, type === 'date' ? { dateStyle: s } : { timeStyle: s }),
  );
}

function pluralRules(locale: string, type: 'cardinal' | 'ordinal'): Intl.PluralRules {
  return cached(`p:${locale}:${type}`, () => new Intl.PluralRules(locale, { type }));
}

function toNumber(value: IcuValue): number {
  if (typeof value === 'number') return value;
  if (typeof value === 'bigint') return Number(value);
  return Number(value ?? 0);
}

function toDate(value: IcuValue): Date {
  return value instanceof Date ? value : new Date(typeof value === 'number' ? value : String(value ?? ''));
}

export function formatIcu(nodes: IcuNode[], locale: string, values: IcuValues = {}, pound?: number): string {
  let out = '';
  for (const node of nodes) {
    switch (node.type) {
      case 'text':
        out += node.value;
        break;
      case 'pound':
        out += pound === undefined ? '#' : numberFormat(locale).format(pound);
        break;
      case 'argument': {
        const value = values[node.name];
        out +=
          value instanceof Date
            ? dateFormat(locale, 'date').format(value)
            : typeof value === 'number' || typeof value === 'bigint'
              ? numberFormat(locale).format(value)
              : String(value ?? '');
        break;
      }
      case 'number':
        out += numberFormat(locale, node.style).format(toNumber(values[node.name]));
        break;
      case 'date':
      case 'time':
        out += dateFormat(locale, node.type, node.style).format(toDate(values[node.name]));
        break;
      case 'plural':
      case 'selectordinal': {
        const n = toNumber(values[node.name]);
        const exact = node.options[`=${n}`];
        const relative = n - node.offset;
        const rule = pluralRules(locale, node.type === 'plural' ? 'cardinal' : 'ordinal').select(relative);
        const branch = exact ?? node.options[rule] ?? (node.options['other'] as IcuNode[]);
        out += formatIcu(branch, locale, values, relative);
        break;
      }
      case 'select': {
        const key = String(values[node.name] ?? '');
        const branch = node.options[key] ?? (node.options['other'] as IcuNode[]);
        out += formatIcu(branch, locale, values, pound);
        break;
      }
    }
  }
  return out;
}
