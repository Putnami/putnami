import { bootstrapServe } from '@putnami/application';
import { app } from './main';

await bootstrapServe(app);
