import type { TrackFn } from '../wire';

/**
 * Returns a function that records one declared action.
 *
 * It imports neither React nor the tracker: it reads
 * `window.__putnamiAnalytics` at call time, which makes it safe on the server
 * (no `window`, so calling it does nothing) and safe on a static page (no
 * tracker, so calling it does nothing). That is why the same file is exported
 * from both package entries — a hook that needed the browser bundle could not
 * be imported by a component that also renders on the server.
 *
 * It is named `useTrack` for ergonomics, not because it uses React state: it
 * has no dependencies, no cleanup, and obeys the rules of hooks trivially.
 *
 * @returns The tracking function; a no-op wherever the tracker is not loaded.
 */
export function useTrack(): TrackFn {
  return (name, props) => {
    const target = (globalThis as { window?: { __putnamiAnalytics?: { track: TrackFn } } }).window;
    target?.__putnamiAnalytics?.track(name, props ?? {});
  };
}
