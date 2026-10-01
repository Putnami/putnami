/**
 * Environment declaration for TypeScript.
 */
declare namespace NodeJS {
  interface ProcessEnv {
    PWD?: string;
    NODE_ENV?: string;
    K_SERVICE?: string;
    PUTNAMI_WORKSPACE_ROOT?: string;
    PUTNAMI_PROJECT_ROOT?: string;
  }
}
