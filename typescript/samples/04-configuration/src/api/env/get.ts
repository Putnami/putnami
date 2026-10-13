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
      appEnv: "APP_ENV is set → its value ('prod' → 'production')",
      production: "NODE_ENV === 'production' or K_SERVICE is set → 'production'",
      local: "Otherwise → 'local'",
    },
    environmentVariables: {
      NODE_ENV: process.env.NODE_ENV || '(not set)',
      APP_ENV: process.env.APP_ENV || '(not set)',
    },
  };
});
