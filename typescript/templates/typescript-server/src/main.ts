import { api, application, http, logger } from '@putnami/application';

export const app = () => application().use(http()).use(logger()).use(api());
