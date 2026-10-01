import { endpoint } from '@putnami/application';
import { UnauthorizedException } from '@putnami/runtime';
import { TENANT_SCOPE } from '../../caller-identity';

// GET /tenant-check — two credentials one alternative requires together.
//
// The `catalog-key` api key and the `tenant` named header form one alternative:
// a consumer that binds only one of them never dispatches the call. The
// provider reports that both arrived and never echoes either value.
export const GET = endpoint()
  .returns({ key: Boolean, tenant: Boolean })
  .mayThrow('Unauthorized')
  .secure({ scopes: [TENANT_SCOPE] })
  .client({
    security: {
      alternatives: [{ allOf: [{ profile: 'catalog-key', scopes: [TENANT_SCOPE] }, { profile: 'tenant' }] }],
    },
    transports: ['rest-json'],
  })
  .handle((ctx) => {
    // The identity resolver carries this scope only when the api key and the
    // tenant arrived together, so reaching the handler is the proof itself.
    if (!ctx.user) {
      throw new UnauthorizedException('the catalog key and the tenant are both required');
    }
    return { key: true, tenant: true };
  });
