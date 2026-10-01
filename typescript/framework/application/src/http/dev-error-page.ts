import { HttpException } from '@putnami/runtime';
import { escapeHtml } from '@putnami/utils';
import type { HttpRequestContext } from './http-context.type';
import { HttpResponse } from './http-response';

interface StackFrame {
  fn: string;
  file: string;
  line: number;
  col: number;
}

/**
 * Whether development error details (stack traces, source, raw messages) may be
 * exposed to clients. Read dynamically — not as a module-load constant — so the
 * documented `PUTNAMI_DEV_ERRORS=false` kill-switch actually takes effect, and
 * so a single predicate gates both the JSON and HTML error paths.
 *
 * Enabled when `NODE_ENV` is not exactly `production` AND `PUTNAMI_DEV_ERRORS`
 * is not set to `'false'`.
 */
export function devErrorsEnabled(): boolean {
  return process.env.NODE_ENV !== 'production' && process.env['PUTNAMI_DEV_ERRORS'] !== 'false';
}

/**
 * Check whether dev error overlays are enabled AND the client accepts
 * `text/html`. When both are true we can render a rich error overlay
 * instead of raw JSON.
 */
export function isDevHtmlRequest(context: HttpRequestContext): boolean {
  if (!devErrorsEnabled()) return false;
  const accept = context.headers.get('Accept') ?? '';
  return accept.includes('text/html');
}

/**
 * Render a self-contained HTML error page for development.
 * Shows the error message, source code context, full stack trace,
 * and request details.
 */
export async function renderDevErrorPage(error: unknown, context: HttpRequestContext): Promise<HttpResponse> {
  const { name, message, stack, statusCode } = extractErrorInfo(error);
  const frames = parseStack(stack);
  const sourceHtml = await renderSourceContext(frames[0]);
  const stackHtml = renderStackTrace(frames);
  const requestHtml = renderRequestDetails(context);

  const html = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${statusCode} — ${escapeHtml(name)}</title>
<style>${CSS}</style>
</head>
<body>
<div class="container">
  <header class="error-header">
    <span class="badge">${statusCode}</span>
    <h1>${escapeHtml(name)}: ${escapeHtml(message)}</h1>
  </header>
  ${sourceHtml}
  <section class="card">
    <h2>Stack Trace</h2>
    ${stackHtml}
  </section>
  <section class="card">
    <h2>Request</h2>
    ${requestHtml}
  </section>
</div>
</body>
</html>`;

  return new HttpResponse(html, {
    status: statusCode,
    headers: { 'Content-Type': 'text/html; charset=utf-8' },
  });
}

// ---------------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------------

function extractErrorInfo(error: unknown): {
  name: string;
  message: string;
  stack: string;
  statusCode: number;
} {
  if (error instanceof HttpException) {
    return {
      name: error.name,
      message: error.message,
      stack: error.stack ?? '',
      statusCode: error.getStatus(),
    };
  }
  if (error instanceof Error) {
    return { name: error.name, message: error.message, stack: error.stack ?? '', statusCode: 500 };
  }
  return { name: 'Error', message: String(error), stack: '', statusCode: 500 };
}

/** Parse V8/Bun stack traces into structured frames. */
function parseStack(stack: string): StackFrame[] {
  const frames: StackFrame[] = [];
  for (const line of stack.split('\n')) {
    const m = line.match(/^\s*at\s+(.*?)\s+\((.+):(\d+):(\d+)\)/) ?? line.match(/^\s*at\s+(.+):(\d+):(\d+)$/);
    if (!m) continue;
    if (m.length === 5) {
      frames.push({ fn: m[1], file: m[2], line: Number(m[3]), col: Number(m[4]) });
    } else if (m.length === 4) {
      frames.push({ fn: '<anonymous>', file: m[1], line: Number(m[2]), col: Number(m[3]) });
    }
  }
  return frames;
}

const ALLOWED_EXTENSIONS = new Set(['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs']);

/** Read source lines around the error location. */
async function renderSourceContext(frame: StackFrame | undefined): Promise<string> {
  if (!frame) return '';
  try {
    // Validate path: only allow source files within the project root
    const projectRoot = process.cwd();
    if (!frame.file.startsWith(projectRoot) || frame.file.includes('..')) return '';
    const ext = frame.file.slice(frame.file.lastIndexOf('.'));
    if (!ALLOWED_EXTENSIONS.has(ext)) return '';

    const content = await Bun.file(frame.file).text();
    const lines = content.split('\n');
    const start = Math.max(0, frame.line - 6);
    const end = Math.min(lines.length, frame.line + 5);
    const shortPath = shortenPath(frame.file);

    let html = `<section class="card"><h2>Source</h2>`;
    html += `<div class="file-path"><a href="${editorLink(frame)}">${escapeHtml(shortPath)}:${frame.line}</a></div>`;
    html += '<pre class="source"><code>';
    for (let i = start; i < end; i++) {
      const lineNum = i + 1;
      const isCurrent = lineNum === frame.line;
      const cls = isCurrent ? ' class="highlight"' : '';
      const marker = isCurrent ? '\u25B8' : ' ';
      html += `<span${cls}><span class="ln">${String(lineNum).padStart(4)} ${marker}</span>${escapeHtml(lines[i])}\n</span>`;
    }
    html += '</code></pre></section>';
    return html;
  } catch {
    return '';
  }
}

function renderStackTrace(frames: StackFrame[]): string {
  if (frames.length === 0) return '<p class="muted">No stack trace available</p>';
  let html = '<ul class="stack">';
  for (const f of frames) {
    const short = shortenPath(f.file);
    html += `<li><span class="fn">${escapeHtml(f.fn)}</span> <a class="loc" href="${editorLink(f)}">${escapeHtml(short)}:${f.line}:${f.col}</a></li>`;
  }
  html += '</ul>';
  return html;
}

function renderRequestDetails(ctx: HttpRequestContext): string {
  const method = ctx.req.method;
  const url = ctx.req.url;
  const params = ctx.params;

  let html = '<dl class="details">';
  html += `<dt>Method</dt><dd>${escapeHtml(method)}</dd>`;
  html += `<dt>URL</dt><dd>${escapeHtml(url)}</dd>`;
  if (ctx.route) {
    html += `<dt>Route</dt><dd>${escapeHtml(ctx.route)}</dd>`;
  }
  if (params && Object.keys(params).length > 0) {
    html += `<dt>Params</dt><dd><pre>${escapeHtml(JSON.stringify(params, null, 2))}</pre></dd>`;
  }

  // Headers (filter out sensitive ones)
  const sensitiveHeaders = new Set(['cookie', 'authorization', 'proxy-authorization', 'set-cookie']);
  const headerEntries: [string, string][] = [];
  ctx.headers.forEach((value, key) => {
    if (!sensitiveHeaders.has(key.toLowerCase())) {
      headerEntries.push([key, value]);
    }
  });
  if (headerEntries.length > 0) {
    html += '<dt>Headers</dt><dd><table class="headers-table">';
    for (const [k, v] of headerEntries) {
      html += `<tr><td>${escapeHtml(k)}</td><td>${escapeHtml(v)}</td></tr>`;
    }
    html += '</table></dd>';
  }

  html += '</dl>';
  return html;
}

/**
 * A VS Code link to the frame's file, line and column. A Windows drive path
 * such as `C:\app\x.ts` becomes `/C:/app/x.ts`, the form the link expects;
 * any other path is kept as it is.
 */
function editorLink(frame: StackFrame): string {
  const file = /^[A-Za-z]:[\\/]/.test(frame.file) ? `/${frame.file.replaceAll('\\', '/')}` : frame.file;
  return `vscode://file${file}:${frame.line}:${frame.col}`;
}

/** Shorten absolute paths to project-relative. */
function shortenPath(filePath: string): string {
  const cwd = process.cwd();
  if (filePath.startsWith(cwd)) {
    return filePath.slice(cwd.length + 1);
  }
  return filePath;
}

// ---------------------------------------------------------------------------
// Styles
// ---------------------------------------------------------------------------

const CSS = `
*{margin:0;padding:0;box-sizing:border-box}
body{background:#1a1a2e;color:#e0e0e0;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Oxygen,Ubuntu,sans-serif;line-height:1.6;padding:2rem}
.container{max-width:960px;margin:0 auto}
.error-header{margin-bottom:1.5rem}
.error-header h1{font-size:1.4rem;font-weight:600;color:#ff6b6b;margin-top:.5rem}
.badge{display:inline-block;background:#ff6b6b;color:#1a1a2e;font-weight:700;font-size:.85rem;padding:.15rem .6rem;border-radius:4px}
.card{background:#16213e;border:1px solid #0f3460;border-radius:8px;padding:1.25rem;margin-bottom:1.25rem}
.card h2{font-size:.95rem;text-transform:uppercase;letter-spacing:.05em;color:#8899aa;margin-bottom:.75rem}
.file-path{margin-bottom:.5rem}
.file-path a{color:#4fc3f7;text-decoration:none;font-family:monospace;font-size:.9rem}
.file-path a:hover{text-decoration:underline}
pre.source{overflow-x:auto;font-size:.85rem;line-height:1.7}
pre.source code{display:block}
pre.source span{display:block;padding:0 .75rem;white-space:pre}
pre.source .highlight{background:rgba(255,107,107,.15);border-left:3px solid #ff6b6b;font-weight:600}
.ln{color:#5c6370;user-select:none;margin-right:.75rem;display:inline-block;min-width:5ch;text-align:right}
.stack{list-style:none}
.stack li{padding:.35rem 0;border-bottom:1px solid #0f3460;font-family:monospace;font-size:.85rem}
.stack li:last-child{border-bottom:none}
.fn{color:#c792ea}
.loc{color:#4fc3f7;text-decoration:none}
.loc:hover{text-decoration:underline}
.details{display:grid;grid-template-columns:auto 1fr;gap:.35rem 1rem;font-size:.9rem}
.details dt{font-weight:600;color:#8899aa}
.details dd{font-family:monospace;word-break:break-all}
.details pre{margin:0;font-size:.85rem}
.headers-table{border-collapse:collapse;width:100%}
.headers-table td{padding:.2rem .5rem;border-bottom:1px solid #0f3460;font-size:.85rem}
.headers-table td:first-child{color:#8899aa;white-space:nowrap}
.muted{color:#5c6370;font-style:italic}
`;
