import { application, api, http, logger, openapi, platform, redirect } from '@putnami/application';

export const app = () =>
  application()
    .use(http().get('/', () => redirect('/tasks')))
    .use(logger())
    .use(platform())
    .use(openapi({ title: 'Tasks API', version: '1.0.0' }))
    .use(api());
