declare namespace NodeJS {
  interface ProcessEnv {
    CONFIG_DATA?: string;
    CONFIG_SERVER_URL?: string;
    NODE_ENV?: string;
    K_SERVICE?: string;
    K_REVISION?: string;
    CI?: string;
  }
}
