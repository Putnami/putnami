import { Config, configToken } from '@putnami/runtime';

export const SiteConfig = Config('app', {});
export const SiteConfigToken = configToken(SiteConfig);
