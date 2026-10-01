import { endpoint } from '@putnami/application';
import { getEnv } from '@putnami/runtime';

// GET /env — Environment detection info
export const GET = endpoint().handle(() => {
  const env = getEnv();

  return {
    currentEnvironment: env,
    configFile: `.env.${env}.yaml`,
    detectionLogic: {
      test: "NODE_ENV === 'test' → 'test'",
      production: "NODE_ENV === 'production' → 'production'",
      local: "Otherwise → 'local'",
    },
    environmentVariables: {
      NODE_ENV: process.env.NODE_ENV || '(not set)',
    },
  };
});
