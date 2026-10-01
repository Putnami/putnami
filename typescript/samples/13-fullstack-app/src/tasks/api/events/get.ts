import { endpoint } from '@putnami/application';
import { Optional } from '@putnami/runtime';
import { getActivity } from '../../../shared/activity.feed';

export const GET = endpoint()
  .query({ limit: Optional(Number) })
  .handle((ctx) => {
    const query = ctx.queryParams();
    return {
      events: getActivity('tasks', query.limit ?? 50),
    };
  });
