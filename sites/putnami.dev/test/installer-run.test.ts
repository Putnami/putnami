import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { application, HttpPlugin, http } from '@putnami/application';
import { joinPath } from '@putnami/utils';
import {
  bakeRunCommand,
  INSTALL_PS1,
  INSTALL_SH,
  installerRun,
  parseRunQuery,
} from '../src/plugins/installer-run.plugin';

const WORKSPACE_ROOT = joinPath(import.meta.dir, '..', '..', '..');
const readScript = (name: string) => readFileSync(joinPath(WORKSPACE_ROOT, 'tooling', 'cli', 'scripts', name), 'utf8');
const INSTALL_SCRIPT = readScript('install.sh');
const INSTALL_PS1_SCRIPT = readScript('install.ps1');
const RUN_PLACEHOLDER_LINE = INSTALL_SH.placeholder;

describe('installer run form', () => {
  describe('parseRunQuery', () => {
    it('treats a query without run as the plain installer', () => {
      expect(parseRunQuery('')).toEqual({ kind: 'absent' });
      expect(parseRunQuery('?')).toEqual({ kind: 'absent' });
      expect(parseRunQuery('?foo=bar')).toEqual({ kind: 'absent' });
      expect(parseRunQuery('?RUN=deploy')).toEqual({ kind: 'absent' });
    });

    it('accepts one command name, with or without the leading ?', () => {
      expect(parseRunQuery('?run=deploy')).toEqual({ kind: 'command', command: 'deploy' });
      expect(parseRunQuery('run=a-b-9')).toEqual({ kind: 'command', command: 'a-b-9' });
      expect(parseRunQuery('?foo=1&run=x')).toEqual({ kind: 'command', command: 'x' });
      const longest = `a${'b'.repeat(63)}`;
      expect(parseRunQuery(`?run=${longest}`)).toEqual({ kind: 'command', command: longest });
    });

    it('rejects every value that is not exactly one command name', () => {
      for (const query of [
        '?run',
        '?run=',
        '?run=Deploy',
        '?run=-deploy',
        '?run=9deploy',
        `?run=a${'b'.repeat(64)}`,
        '?run=deploy&run=deploy',
        '?run=deploy;id',
        '?run=deploy%3Bid',
        '?run=$(id)',
        '?run=%24(id)',
        '?run=`id`',
        '?run=%60id%60',
        '?run=deploy"',
        '?run=deploy%22',
        "?run=deploy'",
        '?run=deploy%27',
        '?run=deploy%0aid',
        '?run=deploy%0A',
        '?run=deploy%0d',
        '?run=deploy+now',
        '?run=deploy%20now',
        '?run=deploy%00',
        '?run=d%C3%A9ploy',
        '?run=deploy\\',
      ]) {
        expect({ query, run: parseRunQuery(query) }).toEqual({ query, run: { kind: 'invalid' } });
      }
    });
  });

  describe('bakeRunCommand', () => {
    it('sets the one placeholder line of the shipped installer and changes nothing else', () => {
      const baked = bakeRunCommand(INSTALL_SCRIPT, 'deploy', INSTALL_SH);
      expect(baked).toBeDefined();
      const before = INSTALL_SCRIPT.split('\n');
      const after = (baked as string).split('\n');
      expect(after.length).toBe(before.length);
      const changed = before.flatMap((line, index) => (line === after[index] ? [] : [[line, after[index]]]));
      expect(changed).toEqual([[RUN_PLACEHOLDER_LINE, 'RUN_COMMAND_DEFAULT="deploy"']]);
    });

    it('sets the one placeholder line of the shipped Windows installer and changes nothing else', () => {
      const baked = bakeRunCommand(INSTALL_PS1_SCRIPT, 'deploy', INSTALL_PS1);
      expect(baked).toBeDefined();
      const before = INSTALL_PS1_SCRIPT.split('\n');
      const after = (baked as string).split('\n');
      expect(after.length).toBe(before.length);
      const changed = before.flatMap((line, index) => (line === after[index] ? [] : [[line, after[index]]]));
      expect(changed).toEqual([["$RunCommandDefault = ''", "$RunCommandDefault = 'deploy'"]]);
      // Windows PowerShell 5.1 reads a response without a charset in the ANSI
      // code page, which reads ASCII unchanged.
      expect([...(baked as string)].every((character) => character.charCodeAt(0) < 0x80)).toBe(true);
    });

    it('refuses a script without exactly one placeholder', () => {
      expect(bakeRunCommand('#!/bin/bash\necho hi\n', 'deploy', INSTALL_SH)).toBeUndefined();
      expect(
        bakeRunCommand(`${RUN_PLACEHOLDER_LINE}\n${RUN_PLACEHOLDER_LINE}\n`, 'deploy', INSTALL_SH),
      ).toBeUndefined();
      // Only the whole line counts: an indented or commented copy is not the placeholder.
      expect(
        bakeRunCommand(`  ${RUN_PLACEHOLDER_LINE}\n# ${RUN_PLACEHOLDER_LINE}\n`, 'deploy', INSTALL_SH),
      ).toBeUndefined();
      const ps1 = INSTALL_PS1.placeholder;
      expect(bakeRunCommand(`${ps1}\n${ps1}\n`, 'deploy', INSTALL_PS1)).toBeUndefined();
      expect(bakeRunCommand(`    ${ps1}\n# ${ps1}\n`, 'deploy', INSTALL_PS1)).toBeUndefined();
      // A checkout with CRLF line endings is refused, not half-baked.
      expect(bakeRunCommand(`${ps1}\r\n`, 'deploy', INSTALL_PS1)).toBeUndefined();
      // Each installer is baked through its own placeholder only.
      expect(bakeRunCommand(INSTALL_SCRIPT, 'deploy', INSTALL_PS1)).toBeUndefined();
      expect(bakeRunCommand(INSTALL_PS1_SCRIPT, 'deploy', INSTALL_SH)).toBeUndefined();
    });

    it('refuses a command the installer would refuse', () => {
      for (const command of ['', 'Deploy', 'deploy;id', '$(id)', 'deploy"', "deploy'", 'deploy\nid']) {
        expect(bakeRunCommand(INSTALL_SCRIPT, command, INSTALL_SH)).toBeUndefined();
        expect(bakeRunCommand(INSTALL_PS1_SCRIPT, command, INSTALL_PS1)).toBeUndefined();
      }
    });
  });

  describe('installerRun', () => {
    // Scripts without the placeholder: the site must refuse to serve them for
    // ?run= rather than serve a script that ignores the command.
    let dir = '';
    let app: ReturnType<typeof application> | undefined;
    let baseUrl = '';

    beforeAll(async () => {
      dir = mkdtempSync(joinPath(tmpdir(), 'installer-run-'));
      writeFileSync(joinPath(dir, 'install.sh'), '#!/bin/bash\necho no placeholder\n');
      writeFileSync(joinPath(dir, 'install.ps1'), "Write-Host 'no placeholder'\n");
      app = application()
        .use(http({ port: 0 }))
        .use(installerRun({ folder: dir }));
      await app.start();
      baseUrl = `http://localhost:${app.getPlugin(HttpPlugin).getServer()?.port}`;
    });

    afterAll(async () => {
      await app?.stop();
      if (dir) rmSync(dir, { recursive: true, force: true });
    });

    it('answers 500 for ?run= when the script does not carry exactly one placeholder', async () => {
      const res = await fetch(`${baseUrl}/install.sh?run=deploy`);
      expect(res.status).toBe(500);
      expect(res.headers.get('content-type')).toContain('text/plain');
      expect(res.headers.get('cache-control')).toBe('no-store');
      const body = await res.text();
      expect(body).toBe('install.sh does not carry exactly one run placeholder\n');
    });

    it('answers 500 for install.ps1?run= when the script does not carry exactly one placeholder', async () => {
      const res = await fetch(`${baseUrl}/install.ps1?run=deploy`);
      expect(res.status).toBe(500);
      expect(res.headers.get('content-type')).toContain('text/plain');
      expect(res.headers.get('cache-control')).toBe('no-store');
      expect(await res.text()).toBe('install.ps1 does not carry exactly one run placeholder\n');
    });

    it('answers 400 for install.ps1 with a run value that is not one command name', async () => {
      const res = await fetch(`${baseUrl}/install.ps1?run=deploy%27%3Bid`);
      expect(res.status).toBe(400);
      expect(res.headers.get('cache-control')).toBe('no-store');
      expect(await res.text()).not.toContain('id');
    });

    it('leaves every other path to the routes after it', async () => {
      for (const path of ['/install.PS1?run=deploy', '/install.ps1.bak?run=deploy', '/docs/install.ps1?run=deploy']) {
        // biome-ignore lint/performance/noAwaitInLoops: one request at a time, so a failure names its path
        const res = await fetch(`${baseUrl}${path}`);
        await res.text();
        expect({ path, status: res.status }).toEqual({ path, status: 404 });
      }
    });
  });
});
