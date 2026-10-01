import { declareEvents } from '@putnami/analytics';

/**
 * The action vocabulary this application records.
 *
 * Declaring up front is what keeps the analytics payload bounded: the browser
 * and `track()` can only ever move a name that already exists here.
 */
export const analyticsEvents = declareEvents({
  task_created: { priority: Number },
  project_archived: {},
});
