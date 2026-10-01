import { island } from '@putnami/web';
import { SearchProvider } from '../components/search-provider';

/**
 * Island wrapper for the Cmd+K search provider. Hydrates on idle to attach the
 * keyboard listener and render the (portal-based) search modal. Router-free: it
 * navigates with a full-page load, which is correct on a zero-base-JS static
 * site. The navbar's search buttons dispatch a synthetic Cmd+K keydown on
 * `document`, which this island's listener picks up across island boundaries.
 */
export default island().load('idle').render(SearchProvider);
