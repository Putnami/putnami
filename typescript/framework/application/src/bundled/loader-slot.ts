import type { Module, Plugin } from '../application';

/**
 * Export key of the generated loader owned by the plugin at `slot` among the
 * plugins of one family: `<family>-loader` for the first, `<family>-<slot>-loader`
 * for the others.
 *
 * The packaged serve entrypoint registers every generated loader under the key
 * the build exported, and each plugin looks up its own. The key therefore has to
 * be computable inside the packaged binary, where no scan folder exists and
 * `import.meta.dir` names the bundle. The slot comes from composition alone (see
 * {@link generatedLoaderSlot}), which the build and the binary share. The first
 * slot keeps the historical single key, so a workload with one plugin of a
 * family exports what it always did (ADR 0006).
 */
export function generatedLoaderKey(family: string, slot: number): string {
  return slot === 0 ? `${family}-loader` : `${family}-${slot}-loader`;
}

/**
 * The plugins of `owner`'s application that own a generated loader of the same
 * family as `plugin`, in module-tree registration order.
 *
 * `ownsLoader` must read configuration only, never the filesystem: the packaged
 * binary has no scan folder, and a predicate that answers differently in the
 * build and in the binary shifts every slot after it. A plugin outside an
 * application tree, or one `ownsLoader` rejects, is alone in its family.
 */
export function generatedLoaderPlugins<T extends Plugin>(
  owner: Module | undefined,
  plugin: T,
  ownsLoader: (candidate: Plugin) => candidate is T,
): T[] {
  const root = typeof owner?.getRoot === 'function' ? owner.getRoot() : undefined;
  if (typeof root?.collectPlugins !== 'function') return [plugin];
  const owners = root
    .collectPlugins()
    .map(({ plugin: candidate }) => candidate)
    .filter(ownsLoader);
  return owners.includes(plugin) ? owners : [plugin];
}

/**
 * The slot of `plugin` among the plugins returned by
 * {@link generatedLoaderPlugins}: 0 for the first, and for a plugin that is
 * alone in its family.
 */
export function generatedLoaderSlot<T extends Plugin>(
  owner: Module | undefined,
  plugin: T,
  ownsLoader: (candidate: Plugin) => candidate is T,
): number {
  return generatedLoaderPlugins(owner, plugin, ownsLoader).indexOf(plugin);
}
