/**
 * Shared message shapes for the stock conversations, declared once so both the
 * client stream and the bidirectional stream publish the same named components
 * and the generated clients carry the same typed messages.
 */

// One delta a consumer sends. Declaring the stream input is what gives the
// generated clients a typed `send`: a consumer that ships the wrong shape fails
// to compile, and the runtime validates the frame against this same schema
// before it leaves.
export const stockDeltaSchema = {
  // Declared as a JSON number for the same reason itemSchema declares `stock`
  // that way: this provider cannot state an integer width today.
  delta: Number,
};

// What the provider answers with: the running total on a bidirectional
// conversation, and the single declared result of a client stream. One schema
// for both directions keeps the two modes comparable.
export const stockTotalSchema = {
  id: String,
  applied: Number,
  total: Number,
};
