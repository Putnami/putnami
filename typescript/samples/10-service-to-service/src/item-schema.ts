import { Optional } from '@putnami/runtime';

// Shared response shape for an item. Declaring the schema once and reusing it
// across endpoints lets the OpenAPI generator promote it to a named component,
// so the generated client (clients/ts) exposes a typed model instead of an
// opaque `Record<string, unknown>`.
export const itemSchema = {
  id: String,
  name: String,
  price: Number,
  // Declared as a JSON number, not Int: the TypeScript provider cannot declare
  // an integer width today and the strict emitters refuse an integer with no
  // format.
  stock: Number,
  // Optional: an absent property and a present one are different answers on
  // the wire, and the generated clients keep that distinction.
  discontinuedAt: Optional(String),
};

// One message of the item history feed. The revision is the provider's own
// position in the feed, so a consumer that reads two of them can see for itself
// that nothing was skipped and nothing arrived twice.
export const itemRevisionSchema = {
  id: String,
  revision: String,
};
