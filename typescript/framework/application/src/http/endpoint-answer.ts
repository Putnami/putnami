import type { HttpRequestContext } from './http-context.type';
import type { RouteHandler } from './route.type';

/**
 * The parts of an endpoint's HTTP handler: the handler it calls (its input
 * validated) and the answer it gives to that handler's result, such as the
 * empty answer of an `undefined` one. A route compiled at start calls the
 * parts in its own continuation, so the endpoint adds no promise of its own.
 */
export interface EndpointAnswer {
  readonly inner: RouteHandler;
  readonly answer: (ctx: HttpRequestContext, result: unknown) => unknown;
}

/**
 * The key an endpoint's handler carries its parts under. `Symbol.for` keeps
 * them readable when generated routes and the root HTTP plugin resolve the
 * framework from two locations.
 */
const ENDPOINT_ANSWER = Symbol.for('putnami.http.endpointAnswer');

type AnsweringHandler = RouteHandler & { [ENDPOINT_ANSWER]?: EndpointAnswer };

/** Records on `handler` the parts it is made of, and returns it. */
export function withEndpointAnswer<H extends RouteHandler>(handler: H, parts: EndpointAnswer): H {
  (handler as AnsweringHandler)[ENDPOINT_ANSWER] = parts;
  return handler;
}

/** The parts `handler` is made of, or `undefined` when it records none. */
export function endpointAnswerOf(handler: RouteHandler): EndpointAnswer | undefined {
  return (handler as AnsweringHandler)[ENDPOINT_ANSWER];
}
