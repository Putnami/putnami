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

type Simplify<T> = { [K in keyof T]: T[K] } & {};

/** The values an ICU message needs, by argument name. */
export type MessageArguments<S extends string> = string extends S
  ? Record<string, unknown>
  : Simplify<Walk<S, [], unknown>>;

/** A formatter for one message: argument-free messages take no values. */
export type MessageFormatter<S extends string> = keyof MessageArguments<S> extends never
  ? () => string
  : (values: MessageArguments<S>) => string;
