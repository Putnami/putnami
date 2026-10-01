import { Constrained, Int, IntWidth } from '@putnami/application';
import { ArrayOf } from '@putnami/runtime';

// One price quote, the payload of the two routes this provider declares
// Connect-only. `units` is a declared 64-bit integer, so the generated clients
// type it `bigint`; `offset` is a declared 32-bit integer carried negative,
// which the protobuf wire must sign-extend. Member names are single lowercase
// words: the descriptor joins to the published schema by jsonName.
export const quoteSchema = {
  id: String,
  units: Int,
  offset: Constrained(Int, IntWidth('int32')),
  tags: ArrayOf(String),
};

// One message of the quote tick stream. The sequence is the provider's own
// position, so a consumer sees for itself that nothing was skipped.
export const quoteTickSchema = {
  id: String,
  sequence: Int,
};

// The declared detail of a missing quote: what a consumer may read, and nothing
// the provider did not choose to publish.
export const missingQuoteSchema = {
  resource: String,
  id: String,
};

/** How many ticks one quote stream delivers before it completes. */
export const QUOTE_TICKS = 3;

// The widest `units` a TypeScript provider can hold exactly: its route
// validator works on JavaScript numbers, so 2^53 - 1 is the ceiling here. The
// Go provider carries the full unsigned 64-bit range.
export const quotes = new Map([['1', { id: '1', units: Number.MAX_SAFE_INTEGER, offset: -7, tags: ['a', 'b'] }]]);
