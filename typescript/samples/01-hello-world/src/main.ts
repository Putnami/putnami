import { application, api, http, logger, platform } from '@putnami/application';
export const app = () => application().use(http()).use(logger()).use(platform()).use(api());
