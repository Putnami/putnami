import type { ReactNode } from 'react';

/**
 * Type-level reading of an ICU message's arguments, so `t.seats({ count: 3 })`
 * fails to compile when `count` is missing or has the wrong type. It mirrors
 * the runtime parser's grammar: a `{` in message text opens an argument, and a
 * `{` inside a plural or select argument opens a branch, whose own text may hold
 * arguments again. It jumps from brace to brace, so its depth is the number of
 * braces, not the length of the message.
 */

type Trim<S extends string> = S extends ` ${infer R}` | `\n${infer R}` | `\t${infer R}`
  ? Trim<R>
  : S extends `${infer R} ` | `${infer R}\n` | `${infer R}\t`
    ? Trim<R>
    : S;

type FirstWord<S extends string> = Trim<S> extends `${infer W},${string}` ? Trim<W> : Trim<S>;

type ValueOf<Kind extends string> = Kind extends 'plural' | 'selectordinal' | 'number'
  ? number | bigint
  : Kind extends 'date' | 'time'
    ? Date | number
    : Kind extends 'select'
      ? string
      : string | number;

type Mode = 'text' | 'options';
type Top<Stack extends Mode[]> = Stack extends [...Mode[], infer T] ? T : 'text';
type Pop<Stack extends Mode[]> = Stack extends [...infer R extends Mode[], Mode] ? R : [];

type Walk<S extends string, Stack extends Mode[], Acc> = S extends `${infer Pre}{${infer After}`
  ? Pre extends `${string}}${infer AfterClose}`
    ? Walk<`${AfterClose}{${After}`, Pop<Stack>, Acc>
    : Top<Stack> extends 'options'
      ? Walk<After, [...Stack, 'text'], Acc>
      : OpenArgument<After, Stack, Acc>
  : S extends `${string}}${infer AfterClose}`
    ? Walk<AfterClose, Pop<Stack>, Acc>
    : Acc;

type OpenArgument<S extends string, Stack extends Mode[], Acc> = S extends `${infer Header}}${infer After}`
  ? Header extends `${string},${string}`
    ? Header extends `${infer Name},${infer Rest}`
      ? FirstWord<Rest> extends infer Kind extends string
        ? Kind extends 'plural' | 'selectordinal' | 'select'
          ? Rest extends `${string},${infer Options}`
            ? Walk<`${Options}}${After}`, [...Stack, 'options'], Acc & { [K in Trim<Name>]: ValueOf<Kind> }>
            : Acc
          : Walk<After, Stack, Acc & { [K in Trim<Name>]: ValueOf<Kind> }>
        : Acc
      : Acc
    : Walk<After, Stack, Acc & { [K in Trim<Header>]: ValueOf<'string'> }>
  : Acc;

/**
 * Drop what the runtime reads as text: `''` (a literal quote) and a quoted run
 * that opens on a brace or an angle bracket (`'{literal}'`, `'<b>'`), so its
 * braces open no argument and its brackets no tag.
 */
type StripQuotes<S extends string> = S extends `${infer A}''${infer B}`
  ? StripQuotes<`${A}${B}`>
  : S extends `${infer A}'${'{' | '}' | '<'}${infer B}`
    ? B extends `${string}'${infer C}`
      ? StripQuotes<`${A}${C}`>
      : S
    : S;

type Chars<S extends string, Acc = never> = S extends `${infer C}${infer R}` ? Chars<R, Acc | C> : Acc;
type IdentStart = Chars<'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_'>;
type IdentChar = IdentStart | Chars<'0123456789'>;
type IsIdentRest<S extends string> = S extends `${infer C}${infer R}`
  ? C extends IdentChar
    ? IsIdentRest<R>
    : false
  : true;
type IsIdent<S extends string> = S extends `${infer C}${infer R}`
  ? C extends IdentStart
    ? IsIdentRest<R>
    : false
  : false;

/** The names of the tags a message opens: `<link>` opens `link`; `</link>` and `a < b` open none. */
type TagNames<S extends string, Acc = never> = S extends `${string}<${infer After}`
  ? TagNames<
      After,
      Acc | (After extends `${infer Name}>${string}` ? (IsIdent<Name> extends true ? Name : never) : never)
    >
  : Acc;

/** Renders what a tag wraps. */
export type TagRenderer = (chunks: ReactNode) => ReactNode;

type TagArguments<S extends string> = [TagNames<S>] extends [never] ? unknown : { [K in TagNames<S>]: TagRenderer };

type Simplify<T> = { [K in keyof T]: T[K] } & {};

/** The values an ICU message needs, by argument name, and a renderer per tag. */
export type MessageArguments<S extends string> = string extends S
  ? Record<string, unknown>
  : Simplify<Walk<StripQuotes<S>, [], unknown> & TagArguments<StripQuotes<S>>>;

/**
 * What a message formats to: a string, or a React node when it has tags. A
 * text the compiler cannot read, a `string` rather than a literal, types as a
 * string.
 */
export type MessageResult<S extends string> = string extends S
  ? string
  : [TagNames<StripQuotes<S>>] extends [never]
    ? string
    : ReactNode;

/** A formatter for one message: argument-free messages take no values. */
export type MessageFormatter<S extends string> = keyof MessageArguments<S> extends never
  ? () => MessageResult<S>
  : (values: MessageArguments<S>) => MessageResult<S>;
