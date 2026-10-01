import { application, http, logger } from '@putnami/application';
import { react } from '@putnami/web';

export const app = () => application().use(logger()).use(http()).use(react());
