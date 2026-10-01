import { endpoint, HttpResponse } from '@putnami/application';
import { Desc, Optional } from '@putnami/runtime';
import { RELEASE_CHANNEL, resolveLatestVersion } from '../../lib/release/latest-version';

const RELEASE_RESPONSE_SCHEMA = {
  channel: Desc('The channel the version is read from.', String),
  version: Desc('The version the channel points at; absent until the registry has answered once.', Optional(String)),
};

// Serves /release.json — the Putnami release the site advertises, read by the
// footer island.
export default endpoint()
  .description('The Putnami version the latest channel points at.')
  .response(200, 'The latest Putnami version', RELEASE_RESPONSE_SCHEMA)
  .cache({ maxAge: 300, sMaxAge: 300, staleWhileRevalidate: 300 })
  .handle(async () => {
    const version = await resolveLatestVersion();
    return HttpResponse.json(version ? { channel: RELEASE_CHANNEL, version } : { channel: RELEASE_CHANNEL });
  });
