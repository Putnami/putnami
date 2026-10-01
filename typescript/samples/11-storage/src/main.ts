import { application, api, http, logger, platform, redirect } from '@putnami/application';

export const app = () =>
  application()
    .use(http().get('/', () => redirect('/files')))
    .use(logger())
    .use(platform())
    .use(api());
