import { endpoint } from '@putnami/application';

export const GET = endpoint().handle(() => ({ status: 'ok' }));
