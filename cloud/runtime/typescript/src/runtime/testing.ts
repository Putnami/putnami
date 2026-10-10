// Test seam for the vendored synchronous-fetch transport. Exposed on a
// dedicated `@putnami/cloud/runtime/testing` subpath rather than the
// production barrel: this package is `private: false`, and swapping the HTTP
// transport on the main entry would let any consumer intercept the config /
// secrets resolve calls at runtime. Test code opts in explicitly.
export { resetSyncFetchForTest, setSyncFetchForTest } from './sync-fetch';
export type { SyncFetchRequest, SyncFetchResponse } from './sync-fetch';
