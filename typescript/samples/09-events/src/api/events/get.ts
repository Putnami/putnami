import { endpoint } from '@putnami/application';
import { getEvents } from '../../store';

// Fetch on demand: query the event store at any time to see what happened.
// This is the pull-based counterpart to the reactive handlers.

export const GET = endpoint().handle(() => {
  const events = getEvents();
  return { events, total: events.length };
});
