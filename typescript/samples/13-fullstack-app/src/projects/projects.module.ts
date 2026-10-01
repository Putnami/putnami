import { api, module } from '@putnami/application';

export const projects = () => module('projects').path('/api/projects').use(api());
