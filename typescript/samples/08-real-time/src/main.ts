import { api, application, http, staticFiles } from '@putnami/application';

export const app = () => application().use(http()).use(api()).use(staticFiles());
