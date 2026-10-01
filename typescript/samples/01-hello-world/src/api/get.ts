import { endpoint } from '@putnami/application';

// GET / — Welcome message
export const GET = endpoint().handle(() => ({
  message: 'Hello from Putnami!',
  timestamp: new Date().toISOString(),
}));
