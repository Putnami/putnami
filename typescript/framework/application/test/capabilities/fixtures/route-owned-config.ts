import { Config, configToken, Optional, Sensitive } from '@putnami/runtime';

export const RouteOwnedConfig = Config('capabilities.routeOwned', {
  token: Optional(Sensitive(String)),
});

configToken(RouteOwnedConfig);
