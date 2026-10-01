export type ActivityModule = 'projects' | 'tasks';

export interface ActivityEvent {
  id: string;
  module: ActivityModule;
  type: string;
  message: string;
  projectId?: string;
  taskId?: string;
  timestamp: number;
}

type ActivityListener = (event: ActivityEvent) => void;

const FEED_MAX_SIZE = 200;

const feeds: Record<ActivityModule, ActivityEvent[]> = {
  projects: [],
  tasks: [],
};

const listeners: Record<ActivityModule, Set<ActivityListener>> = {
  projects: new Set<ActivityListener>(),
  tasks: new Set<ActivityListener>(),
};

export function publishActivity(
  module: ActivityModule,
  event: Omit<ActivityEvent, 'id' | 'module' | 'timestamp'>,
): ActivityEvent {
  const enriched: ActivityEvent = {
    id: crypto.randomUUID(),
    module,
    timestamp: Date.now(),
    ...event,
  };

  const feed = feeds[module];
  feed.push(enriched);
  if (feed.length > FEED_MAX_SIZE) {
    feed.splice(0, feed.length - FEED_MAX_SIZE);
  }

  for (const listener of listeners[module]) {
    listener(enriched);
  }

  return enriched;
}

export function getActivity(module: ActivityModule, limit = 25): ActivityEvent[] {
  return feeds[module].slice(-Math.max(1, limit));
}

export function subscribeActivity(module: ActivityModule, listener: ActivityListener): () => void {
  listeners[module].add(listener);
  return () => listeners[module].delete(listener);
}
