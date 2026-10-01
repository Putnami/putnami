/**
 * The run form of the CLI installers: `/install.sh?run=<command>` and
 * `/install.ps1?run=<command>`.
 *
 * `curl -fsSL "https://putnami.dev/install.sh?run=<command>" | bash`, and on
 * Windows `irm "https://putnami.dev/install.ps1?run=<command>" | iex`, install the
 * CLI, pin the extension that `/install-commands.txt` names for `<command>`,
 * and run `putnami <command>` in the caller's directory. A script piped into
 * bash or iex receives no arguments, so the command travels inside the script:
 * this plugin serves the bytes of the installer with the value of its one
 * placeholder line, `RUN_COMMAND_DEFAULT=""` in install.sh and
 * `$RunCommandDefault = ''` in install.ps1, set to the command. Each script
 * holds the value to the same rule and lets `--run` and `PUTNAMI_RUN` override
 * it (tooling/cli/doc/22-installing-the-cli.md).
 *
 * - With no `run` in the query, the request falls through to the staticFiles
 *   route, so the installer is served byte-identical, with the same headers.
 * - `run` must appear once and match `^[a-z][a-z0-9-]{0,63}$`; anything else is
 *   a 400 that does not echo the value. Only a matching value is ever written
 *   into the script, between double quotes in bash or single quotes in
 *   PowerShell, where none of its characters is special.
 * - A script without exactly one placeholder line is a 500: the site never
 *   serves a script that would silently ignore the requested command.
 * - The response keeps the static route's Content-Type and Cache-Control. The
 *   max-age comes from the `putnami.static` config section, which is what the
 *   generated static route for the installers reads (it does not see the
 *   options passed to `staticFiles()`). The ETag is derived from the served
 *   bytes, so each command has its own.
 *
 * The plugin registers no route. Route matching ignores the query, so the
 * `exact /install.sh` and `exact /install.ps1` facts the public-surface plugin
 * declares cover every variant, and a second route for the same path would
 * fail the inventory with `http_routes.duplicate_route`.
 */
import { createHash } from 'node:crypto';
import { HttpPlugin, HttpResponse, type Module, type Plugin, publicFolder, StaticConfig } from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { fileExists, joinPath } from '@putnami/utils';

/** The command names the installers accept; the scripts apply the same rule. */
export const RUN_COMMAND_PATTERN = /^[a-z][a-z0-9-]{0,63}$/;

/** An installer with a run form, and the one line of it whose value the site sets. */
export interface RunInstaller {
  /** The script's name in the public folder, which is also its URL path. */
  readonly script: string;
  /** The Content-Type the static route serves the script with. */
  readonly contentType: string;
  /** The placeholder line, as the repository script carries it. */
  readonly placeholder: string;
  /** The placeholder line with its value set to `command`. */
  readonly bakedLine: (command: string) => string;
}

export const INSTALL_SH: RunInstaller = {
  script: 'install.sh',
  contentType: 'application/x-sh',
  placeholder: 'RUN_COMMAND_DEFAULT=""',
  bakedLine: (command) => `RUN_COMMAND_DEFAULT="${command}"`,
};

export const INSTALL_PS1: RunInstaller = {
  script: 'install.ps1',
  // The static route's MIME table does not list .ps1. irm decodes the body as
  // text whatever the type, and iex runs it.
  contentType: 'application/octet-stream',
  placeholder: "$RunCommandDefault = ''",
  bakedLine: (command) => `$RunCommandDefault = '${command}'`,
};

const RUN_INSTALLERS: readonly RunInstaller[] = [INSTALL_SH, INSTALL_PS1];

export type RunQuery = { kind: 'absent' } | { kind: 'invalid' } | { kind: 'command'; command: string };

/** Read `run` from a raw query string (with or without its leading `?`). */
export function parseRunQuery(query: string): RunQuery {
  const values = new URLSearchParams(query).getAll('run');
  if (values.length === 0) return { kind: 'absent' };
  const command = values[0];
  if (values.length !== 1 || command === undefined || !RUN_COMMAND_PATTERN.test(command)) {
    return { kind: 'invalid' };
  }
  return { kind: 'command', command };
}

/**
 * Set the placeholder of `script`, the text of `installer`, to `command` and
 * change nothing else. Returns undefined when the command does not match the
 * rule or the script does not carry exactly one placeholder line of that
 * installer. A line ending in a carriage return is not the placeholder.
 */
export function bakeRunCommand(script: string, command: string, installer: RunInstaller): string | undefined {
  if (!RUN_COMMAND_PATTERN.test(command)) return undefined;
  const lines = script.split('\n');
  const placeholders = lines.flatMap((line, index) => (line === installer.placeholder ? [index] : []));
  const [index] = placeholders;
  if (placeholders.length !== 1 || index === undefined) return undefined;
  lines[index] = installer.bakedLine(command);
  return lines.join('\n');
}

function plainText(status: number, message: string): HttpResponse {
  return new HttpResponse(message, {
    status,
    headers: { 'Content-Type': 'text/plain; charset=utf-8', 'Cache-Control': 'no-store' },
  });
}

/**
 * The response for a baked script, with the static route's headers and an
 * ETag of its own: 304 for a matching If-None-Match, and no body for HEAD.
 */
function bakedResponse(
  baked: string,
  installer: RunInstaller,
  cacheControl: string,
  method: string,
  ifNoneMatch: string | null,
): HttpResponse {
  const etag = `"${createHash('sha256').update(baked, 'utf8').digest('hex').slice(0, 32)}"`;
  if (ifNoneMatch === etag) {
    return new HttpResponse(undefined, { status: 304, headers: { ETag: etag } });
  }
  const headers = { 'Content-Type': installer.contentType, 'Cache-Control': cacheControl, ETag: etag };
  if (method === 'HEAD') {
    return new HttpResponse(undefined, {
      status: 200,
      headers: { ...headers, 'Content-Length': String(Buffer.byteLength(baked, 'utf8')) },
    });
  }
  return new HttpResponse(baked, { headers });
}

export interface InstallerRunOptions {
  /** The folder the installers are read from. Defaults to the generated public folder. */
  folder?: string;
}

class InstallerRunPlugin implements Plugin {
  constructor(private readonly options: InstallerRunOptions) {}

  async warmup(app: Module): Promise<void> {
    const httpPlugin = await app.ensurePlugin(HttpPlugin);
    const folder = this.options.folder ?? publicFolder();
    const cacheControl = `public, max-age=${useConfig(StaticConfig).cacheMaxAge}`;

    httpPlugin.use(async (ctx, next) => {
      if (ctx.method !== 'GET' && ctx.method !== 'HEAD') return next();
      const path = ctx.path();
      const installer = RUN_INSTALLERS.find((candidate) => candidate.script === path);
      if (installer === undefined) return next();
      const run = parseRunQuery(ctx.query());
      if (run.kind === 'absent') return next();
      if (run.kind === 'invalid') {
        return plainText(400, 'run must be one command name matching ^[a-z][a-z0-9-]{0,63}$\n');
      }
      const scriptPath = joinPath(folder, installer.script);
      if (!fileExists(scriptPath)) return next();

      const baked = bakeRunCommand(await Bun.file(scriptPath).text(), run.command, installer);
      if (baked === undefined) {
        return plainText(500, `${installer.script} does not carry exactly one run placeholder\n`);
      }
      return bakedResponse(baked, installer, cacheControl, ctx.method, ctx.headers.get('If-None-Match'));
    });
  }
}

export function installerRun(options: InstallerRunOptions = {}): InstallerRunPlugin {
  return new InstallerRunPlugin(options);
}
