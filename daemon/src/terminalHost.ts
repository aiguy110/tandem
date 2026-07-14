// TerminalHost — daemon-owned terminal execution & buffering (docs/agent-adapter.md,
// docs/architecture.md ACP terminal caveat). Services ACP `terminal/*` on an
// agent's behalf. The daemon owns the process and the output buffer, which is
// exactly the correct side of the durability line: buffers persist past
// `terminal/release`, so re-capture on reconnect is native.
//
// Two buffers, deliberately independent:
//   * ACP view (`output`)      — honors the per-terminal `outputByteLimit`, and
//                                truncates FROM THE START at a UTF-8 char
//                                boundary, with the ACP `truncated` flag.
//   * daemon scrollback        — a larger, configurable cap kept regardless of
//     (`scrollback`)             the ACP limit, so the UI/reconnect sees more
//                                than the agent's own bounded view.
//
// Every output chunk is also emitted as a normalized `terminal_output`
// AgentEvent (via the injected `emit`), so it flows to the `terminals` channel
// and is persisted in the session log like everything else.
//
// node-pty is an optionalDependency: we prefer it (real pty semantics) and
// degrade to child_process pipes if it fails to load.

import { spawn as cpSpawn, type ChildProcess } from 'node:child_process';
import { createRequire } from 'node:module';
import type { AgentEvent, TermExit, TerminalCreateOpts, TerminalHost as ITerminalHost } from './types.ts';

// Optional node-pty (optionalDependency). Preferred for real pty semantics; a
// failed native build degrades to child_process pipes below. TANDEM_NO_PTY=1
// forces the fallback (used to de-risk the degraded path).
let pty: typeof import('node-pty') | null = null;
if (!process.env.TANDEM_NO_PTY) {
  try {
    pty = createRequire(import.meta.url)('node-pty');
  } catch {
    pty = null;
  }
}

const DEFAULT_ACP_LIMIT = 1 << 20; // 1 MiB — generous ACP view when the agent omits one
const DEFAULT_SCROLLBACK_CAP = 8 << 20; // 8 MiB daemon scrollback per terminal

// Advance `start` forward until it lands on a UTF-8 lead byte (not a 0b10xxxxxx
// continuation byte), so truncation never splits a multibyte character.
function charBoundary(buf: Buffer, start: number): number {
  let s = start;
  while (s < buf.length && (buf[s] & 0xc0) === 0x80) s++;
  return s;
}

interface Term {
  id: string;
  // one process handle — exactly one of these is set
  pty?: import('node-pty').IPty;
  child?: ChildProcess;
  scrollback: Buffer; // daemon view (bounded by scrollbackCap)
  scrollbackTruncated: boolean;
  produced: number; // total bytes ever produced
  acpLimit: number; // per-terminal ACP outputByteLimit
  scrollbackCap: number;
  exit: TermExit | null;
  exitWaiters: ((e: TermExit) => void)[];
  released: boolean;
  killed: boolean;
}

export class TerminalHost implements ITerminalHost {
  private terms = new Map<string, Term>();
  private counter = 0;

  constructor(
    private opts: { defaultCwd: string; emit: (ev: AgentEvent) => void; scrollbackCap?: number },
  ) {}

  async create(o: TerminalCreateOpts): Promise<string> {
    const id = `term_${++this.counter}`;
    const cwd = o.cwd || this.opts.defaultCwd;
    const env: Record<string, string> = { ...(process.env as Record<string, string>) };
    for (const e of o.env ?? []) env[e.name] = e.value;

    const t: Term = {
      id,
      scrollback: Buffer.alloc(0),
      scrollbackTruncated: false,
      produced: 0,
      acpLimit: o.outputByteLimit ?? DEFAULT_ACP_LIMIT,
      scrollbackCap: this.opts.scrollbackCap ?? DEFAULT_SCROLLBACK_CAP,
      exit: null,
      exitWaiters: [],
      released: false,
      killed: false,
    };
    this.terms.set(id, t);

    if (pty) {
      const p = pty.spawn(o.command, o.args ?? [], { cwd, env, name: 'xterm-256color', cols: 80, rows: 24 });
      t.pty = p;
      p.onData((d: string) => this.onChunk(t, Buffer.from(d, 'utf8')));
      p.onExit(({ exitCode, signal }) => this.onExit(t, exitCode, signal ? String(signal) : null));
    } else {
      const c = cpSpawn(o.command, o.args ?? [], { cwd, env, stdio: ['ignore', 'pipe', 'pipe'] });
      t.child = c;
      c.stdout?.on('data', (d: Buffer) => this.onChunk(t, d));
      c.stderr?.on('data', (d: Buffer) => this.onChunk(t, d));
      c.on('error', (err) => this.onChunk(t, Buffer.from(`\n[terminal spawn error] ${err.message}\n`, 'utf8')));
      c.on('exit', (code, signal) => this.onExit(t, code, signal ? String(signal) : null));
    }
    return id;
  }

  private onChunk(t: Term, data: Buffer): void {
    t.produced += data.length;
    // Append to the daemon scrollback, trimming from the front past the cap.
    t.scrollback = Buffer.concat([t.scrollback, data]);
    if (t.scrollback.length > t.scrollbackCap) {
      const cut = charBoundary(t.scrollback, t.scrollback.length - t.scrollbackCap);
      t.scrollback = t.scrollback.subarray(cut);
      t.scrollbackTruncated = true;
    }
    // Normalized event for the terminals channel (streamed, not truncated here).
    const ev: AgentEvent = { kind: 'terminal_output', termId: t.id, chunk: data.toString('utf8'), truncated: t.scrollbackTruncated };
    this.opts.emit(ev);
  }

  private onExit(t: Term, code: number | null, signal: string | null): void {
    if (t.exit) return;
    t.exit = { exitCode: code, signal };
    const waiters = t.exitWaiters;
    t.exitWaiters = [];
    for (const w of waiters) w(t.exit);
  }

  // ---- ACP terminal/output view (honors outputByteLimit + truncated) ----
  output(termId: string): { output: string; truncated: boolean; exitStatus: TermExit | null } {
    const t = this.must(termId);
    let view = t.scrollback;
    if (view.length > t.acpLimit) {
      view = view.subarray(charBoundary(view, view.length - t.acpLimit));
    }
    // Truncated if the returned view is smaller than everything ever produced.
    const truncated = view.length < t.produced;
    return { output: view.toString('utf8'), truncated, exitStatus: t.exit };
  }

  // ---- daemon scrollback (independent of the ACP limit) ----
  scrollback(termId: string): { output: string; truncated: boolean } {
    const t = this.must(termId);
    return { output: t.scrollback.toString('utf8'), truncated: t.scrollbackTruncated };
  }

  waitForExit(termId: string): Promise<TermExit> {
    const t = this.must(termId);
    if (t.exit) return Promise.resolve(t.exit);
    return new Promise((res) => t.exitWaiters.push(res));
  }

  kill(termId: string): void {
    const t = this.must(termId);
    t.killed = true;
    if (t.pty) {
      try {
        t.pty.kill();
      } catch {
        /* already gone */
      }
    } else if (t.child && !t.child.killed) {
      t.child.kill('SIGKILL');
    }
  }

  // ACP terminal/release: free the *process* resources but KEEP the buffer, so
  // the output stays retrievable (native reconnect re-capture).
  release(termId: string): void {
    const t = this.terms.get(termId);
    if (!t) return;
    t.released = true;
    // If still running, killing on release matches ACP (agent is done with it).
    if (!t.exit) this.kill(termId);
  }

  exitStatus(termId: string): TermExit | null {
    return this.terms.get(termId)?.exit ?? null;
  }
  has(termId: string): boolean {
    return this.terms.has(termId);
  }

  // Full teardown (agent disposed): actually stop any live processes.
  disposeAll(): void {
    for (const t of this.terms.values()) {
      if (!t.exit) {
        if (t.pty) {
          try {
            t.pty.kill();
          } catch {
            /* ignore */
          }
        } else if (t.child && !t.child.killed) {
          t.child.kill('SIGKILL');
        }
      }
    }
  }

  private must(termId: string): Term {
    const t = this.terms.get(termId);
    if (!t) throw new Error(`no such terminal: ${termId}`);
    return t;
  }

  usesPty(): boolean {
    return pty != null;
  }
}
