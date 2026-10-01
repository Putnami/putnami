import { endpoint } from '@putnami/application';
import { UnauthorizedException } from '@putnami/runtime';
import { CALLER_SCOPE } from '../../caller-identity';

// GET /whoami — the caller's user identity, forwarded from the consumer's own
// inbound request.
//
// The operation declares the `user` profile, the consumer's binding opts in,
// and no consumer code writes an Authorization header. The provider names the
// subject and never echoes the token.
export const GET = endpoint()
  .returns({ subject: String })
  .mayThrow('Unauthorized')
  .secure({ scopes: [CALLER_SCOPE] })
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'user', scopes: [CALLER_SCOPE] }] }] },
    transports: ['rest-json'],
  })
  .handle((ctx) => {
    // The identity resolver named the user from the token the consumer
    // forwarded; the token itself never reaches this handler or the response.
    const subject = ctx.user?.sub;
    if (!subject) {
      throw new UnauthorizedException('unknown caller');
    }
    return { subject };
  });
