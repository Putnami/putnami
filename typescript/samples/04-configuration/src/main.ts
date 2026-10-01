import { application, api, config, http, logger } from '@putnami/application';

export const app = () => application().use(http()).use(logger()).use(config()).use(api());
