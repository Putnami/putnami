import { describe, expect, it } from 'bun:test';
import { Config, configToken, Default, getRegisteredConfigDefinitions, Optional, Sensitive } from '@putnami/runtime';
import { application } from '../../src/application';
import { extractConfigSchema } from '../../src/config/config-schema-extract';

// registerContributedConfigs() writes into the process-global config registry,
// so these blocks use Optional/Default fields — if they leak into a later test
// that starts an app, they can't fail config validation (the convention
// config-token.test.ts follows).
describe('Application.registerContributedConfigs', () => {
  it("aggregates a ConfigContributor plugin's blocks into the published schema", () => {
    const CoreConfig = Config('contrib-app-core', {
      host: Default(String, 'localhost'),
      clientSecret: Optional(Sensitive(String)),
    });
    const corePlugin = { name: 'core-lib', configDefinitions: () => [CoreConfig] };

    const app = application().use(corePlugin);
    app.registerContributedConfigs();

    const manifest = extractConfigSchema({
      appName: 'contrib-app',
      version: '0.0.0',
      definitions: getRegisteredConfigDefinitions(),
      // A test manifest never belongs in the committed `schema/` directory.
      output: false,
    });
    const block = manifest?.configs.find((c) => c.path === 'contrib-app-core');
    if (!block) throw new Error('expected the contributed core block in the schema');

    // The library-owned sensitive field survives aggregation — that's what
    // makes a library-owned secret publishable from the workload.
    const secret = block.fields.find((f) => f.name === 'clientSecret');
    expect(secret?.sensitive).toBe(true);
  });

  it('throws when a dependency claims a path the workload already defines', () => {
    configToken(Config('contrib-app-conflict', { b: Optional(String) })); // workload's own block
    const dependency = Config('contrib-app-conflict', { a: Optional(String) });
    const app = application().use({ name: 'dep', configDefinitions: () => [dependency] });
    expect(() => app.registerContributedConfigs()).toThrow(/contrib-app-conflict/);
  });
});
