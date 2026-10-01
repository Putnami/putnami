import { styled } from '@putnami/ui';
import { useEffect, useRef, useState } from 'react';

// ─── Terminal Script ─────────────────────────────────────
// Edit this array to change the terminal animation.
//
// Actions:
//   type   — types text char-by-char with a zsh prompt
//   print  — prints a line instantly
//   pause  — waits before the next step
//
// Styles for print: 'success' (green ✓), 'info' (cyan →), 'warn' (amber ⚠),
// 'muted' (gray)
// Prompt types: 'clean' (default), 'git' (with branch indicator)

type PromptType = 'clean' | 'git';

type Step =
  | { action: 'type'; text: string; prompt?: PromptType }
  | { action: 'print'; text: string; style?: 'success' | 'info' | 'warn' | 'muted' }
  | { action: 'pause'; ms: number };

const SCRIPT: Step[] = [
  // ── Install ──
  { action: 'type', text: 'curl -fsSL https://putnami.dev/install.sh | bash' },
  { action: 'pause', ms: 300 },
  { action: 'print', text: '→ Downloading Putnami CLI...', style: 'info' },
  { action: 'print', text: '✓ Integrity verified (sha256:c26ef48bd6e4...)', style: 'success' },
  { action: 'print', text: '✓ putnami now runs ~/.local/bin/putnami', style: 'success' },
  { action: 'print', text: '' },
  { action: 'pause', ms: 900 },

  // ── Init ──
  { action: 'type', text: 'putnami init --project webapp --extension ts' },
  { action: 'pause', ms: 400 },
  { action: 'print', text: '✓ Workspace ready', style: 'success' },
  { action: 'print', text: '✓ webapp · @putnami/typescript', style: 'success' },
  { action: 'print', text: '' },
  { action: 'pause', ms: 900 },

  // ── Serve ──
  { action: 'type', text: 'putnami serve webapp' },
  { action: 'pause', ms: 400 },
  { action: 'print', text: 'build(webapp):  generated  (426ms)', style: 'muted' },
  { action: 'print', text: 'serve(webapp):  \u{1F525} warmed up 51ms' },
  { action: 'print', text: 'serve(webapp):  \u26A1\uFE0F listening http://localhost:3000' },
  { action: 'print', text: '' },
  { action: 'pause', ms: 900 },

  // ── Blast radius ──
  { action: 'type', text: 'putnami build --impacted', prompt: 'git' },
  { action: 'pause', ms: 400 },
  { action: 'print', text: '  \u2713 2 of 7 projects impacted  (1.2s)', style: 'success' },
  { action: 'print', text: '' },
  { action: 'pause', ms: 900 },

  // ── Agent orientation ──
  { action: 'type', text: 'putnami context pack --project webapp', prompt: 'git' },
  { action: 'pause', ms: 400 },
  {
    action: 'print',
    text: '  \u2713 webapp/.gen/agent-context.json  by reference \u00B7 0 file contents',
    style: 'success',
  },
  { action: 'print', text: '' },
  { action: 'pause', ms: 900 },

  // ── Production readiness ──
  { action: 'type', text: 'putnami doctor --profile production', prompt: 'git' },
  { action: 'pause', ms: 400 },
  { action: 'print', text: '  \u2713 14 checks passed', style: 'success' },
  { action: 'print', text: '  \u26A0 1 finding \u2014 http.routes.public_edge_unset', style: 'warn' },
  { action: 'print', text: '    remediation: declare visibility in the route inventory', style: 'muted' },
  { action: 'pause', ms: 5000 },
];

// ─── Timing ──────────────────────────────────────────────

const TYPE_SPEED = 50;
const TYPE_JITTER = 30;
const PRINT_DELAY = 80;
const RESTART_DELAY = 4000;
const START_DELAY = 600;

// ─── Prompt ──────────────────────────────────────────────

function ZshPrompt({ type }: { type: PromptType }) {
  return (
    <span className='zsh-prompt'>
      <span className='prompt-dir'>~/acme</span>
      {type === 'git' ? <span className='prompt-branch'> (feat/checkout)</span> : null}
      <span className='prompt-sign'> $ </span>
    </span>
  );
}

// ─── Component ───────────────────────────────────────────

type Line = { id: number; text: string; prompt?: PromptType; style?: string };

export function HeroTerminal() {
  const [lines, setLines] = useState<Line[]>([]);
  const [typing, setTyping] = useState<{ text: string; prompt: PromptType } | null>(null);
  const [idle, setIdle] = useState(false);
  const [idlePrompt, setIdlePrompt] = useState<PromptType>('clean');
  const [reduced, setReduced] = useState(false);
  const ref = useRef({
    step: 0,
    char: 0,
    lineId: 0,
    lastPrompt: 'clean' as PromptType,
    timer: undefined as ReturnType<typeof setTimeout> | undefined,
  });
  const bodyRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // Respect prefers-reduced-motion: render every line at once, no typewriter,
    // no restart loop. The typewriter is the brand's one animated artifact.
    const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false;
    if (reduce) {
      const allLines: Line[] = [];
      let lastPrompt: PromptType = 'clean';
      for (const s of SCRIPT) {
        if (s.action === 'type') {
          lastPrompt = s.prompt ?? 'clean';
          allLines.push({ id: ref.current.lineId++, text: s.text, prompt: lastPrompt });
        } else if (s.action === 'print') {
          allLines.push({ id: ref.current.lineId++, text: s.text, style: s.style });
        }
      }
      setLines(allLines);
      setIdlePrompt(lastPrompt);
      setIdle(true);
      setReduced(true);
      return;
    }

    function tick() {
      const s = SCRIPT[ref.current.step];

      if (!s) {
        setTyping(null);
        setIdlePrompt(ref.current.lastPrompt);
        setIdle(true);
        ref.current.timer = setTimeout(() => {
          setLines([]);
          setTyping(null);
          setIdle(false);
          ref.current.step = 0;
          ref.current.char = 0;
          ref.current.lastPrompt = 'clean';
          ref.current.timer = setTimeout(tick, 400);
        }, RESTART_DELAY);
        return;
      }

      if (s.action === 'type') {
        const c = ref.current.char;
        const prompt = s.prompt ?? 'clean';
        ref.current.lastPrompt = prompt;
        setTyping({ text: s.text.slice(0, c), prompt });
        ref.current.char++;

        if (c < s.text.length) {
          ref.current.timer = setTimeout(tick, TYPE_SPEED + Math.random() * TYPE_JITTER);
        } else {
          const id = ref.current.lineId++;
          setLines((prev) => [...prev, { id, text: s.text, prompt }]);
          setTyping(null);
          ref.current.step++;
          ref.current.char = 0;
          ref.current.timer = setTimeout(tick, 100);
        }
      } else if (s.action === 'print') {
        const id = ref.current.lineId++;
        setLines((prev) => [...prev, { id, text: s.text, style: s.style }]);
        ref.current.step++;
        ref.current.timer = setTimeout(tick, PRINT_DELAY);
      } else {
        ref.current.step++;
        ref.current.timer = setTimeout(tick, s.ms);
      }
    }

    ref.current.timer = setTimeout(tick, START_DELAY);
    return () => clearTimeout(ref.current.timer);
  }, []);

  // Follow the output. The body is a fixed-height scroller, so once the script
  // runs past it the newest line is typed below the fold and the terminal looks
  // frozen. Pin to the bottom after every render — one per typed character —
  // by writing scrollTop directly, so only this container moves and the page
  // never jumps under the reader.
  //
  // Skipped under reduced motion: there the whole script is printed at once and
  // the reader starts at the top of it, not at the end.
  useEffect(() => {
    if (reduced) return;
    const body = bodyRef.current;
    if (body) body.scrollTop = body.scrollHeight;
  });

  return (
    <Terminal role='img' aria-label='Putnami terminal demo'>
      <Chrome>
        <Dots>
          <Dot style={{ background: 'var(--term-branch)' }} />
          <Dot style={{ background: 'var(--term-warn)' }} />
          <Dot style={{ background: 'var(--term-prompt)' }} />
        </Dots>
        <Title>putnami — zsh</Title>
      </Chrome>
      <Body ref={bodyRef}>
        {lines.map((line) => (
          <div key={line.id} className={line.style ?? ''}>
            {line.prompt ? <ZshPrompt type={line.prompt} /> : null}
            {line.text}
          </div>
        ))}
        {typing !== null ? (
          <div>
            <ZshPrompt type={typing.prompt} />
            {typing.text}
            <span className='cursor solid'>▋</span>
          </div>
        ) : null}
        {idle ? (
          <div>
            <ZshPrompt type={idlePrompt} />
            <span className='cursor'>▋</span>
          </div>
        ) : null}
      </Body>
    </Terminal>
  );
}

// ─── Styles ──────────────────────────────────────────────

const Terminal = styled.div`
  border-radius: var(--radius-md);
  border: 1px solid var(--color-border);
  overflow: hidden;
  background: linear-gradient(135deg, var(--term-bg), var(--term-bg-2));
`;

const Chrome = styled.div`
  background: var(--term-chrome);
  padding: 10px 16px;
  display: flex;
  align-items: center;
  position: relative;
`;

const Dots = styled.div`
  display: flex;
  gap: 8px;
`;

const Dot = styled.span`
  width: 12px;
  height: 12px;
  border-radius: 50%;
  display: block;
`;

const Title = styled.span`
  position: absolute;
  left: 50%;
  transform: translateX(-50%);
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  font-size: 13px;
  color: var(--term-fg-muted);
`;

const Body = styled.div`
  padding: 20px 24px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  font-size: 15px;
  line-height: 1.7;
  color: var(--term-fg);
  height: 300px;
  overflow-y: auto;
  overflow-x: auto;
  scrollbar-width: none;

  &::-webkit-scrollbar {
    display: none;
  }

  & > div {
    white-space: pre;
    min-height: 1.7em;
  }

  .prompt-dir {
    color: var(--term-dir);
  }

  .prompt-branch,
  .prompt-sign {
    color: var(--term-fg-muted);
  }

  .success {
    color: var(--term-prompt);
  }

  .info {
    color: var(--term-info);
  }

  .warn {
    color: var(--term-warn);
  }

  .muted {
    color: var(--term-fg-muted);
  }

  .cursor {
    color: var(--term-fg);
    animation: terminal-blink 1s step-end infinite;
  }

  .cursor.solid {
    animation: none;
  }

  @keyframes terminal-blink {
    50% {
      opacity: 0;
    }
  }

  @media (max-width: 640px) {
    font-size: 13px;
    padding: 16px;
    height: 260px;

    /* The scrollbar is hidden, so a clipped command reads as truncated output.
       Wrap like a real terminal instead. */
    & > div {
      white-space: pre-wrap;
      word-break: break-word;
    }
  }
`;
