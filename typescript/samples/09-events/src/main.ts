import { api, application, http, logger, redirect } from '@putnami/application';
import { events } from '@putnami/events';

export const app = () =>
  application()
    .use(http().get('/', () => redirect('/events')))
    .use(logger())
    .use(api())
    .use(events());
