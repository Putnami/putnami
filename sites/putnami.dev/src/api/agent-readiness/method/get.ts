import { endpoint, HttpResponse } from '@putnami/application';

/**
 * The stable public name of the agent-readiness method page.
 *
 * `putnami agent-readiness` reports and landing pages link here. The method
 * itself is a documentation page, so this route only points at it: the page can
 * move inside the docs without breaking a link that is already printed.
 */
const AGENT_READINESS_METHOD_PAGE = '/docs/platform/intelligence/agent-readiness-method';

export default endpoint()
  .description('Redirect to the agent-readiness method documentation page.')
  .response(302, 'Temporary redirect to the agent-readiness method page')
  .cache({ maxAge: 3600, sMaxAge: 3600, staleWhileRevalidate: 3600 })
  .handle(() => HttpResponse.redirect(AGENT_READINESS_METHOD_PAGE, 302));
