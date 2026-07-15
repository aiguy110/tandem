// PTY adapter: the daemon owns a real pty master fd; the child (a TUI-only agent
// or the user's escape-hatch shell) never sees SIGHUP when a browser disconnects.
// node-pty is an optional dependency — spawn() fails gracefully if it's absent.

import { AsyncQueue } from './asyncQueue.ts';
import type { AgentAdapter, AgentEvent, SpawnOpts } from './types.ts';
import { spawn as spawnChild, type ChildProcessWithoutNullStreams } from 'node:child_process';

const shellQuote = (value: string): string => `'${value.replaceAll("'", "'\\''")}'`;

export class PtyAdapter implements AgentAdapter {
  readonly capabilities = { structured: false, terminals: false, loadSession: false, fs: false, image: false };
  private q = new AsyncQueue<AgentEvent>();
  private proc: any;
  private child?: ChildProcessWithoutNullStreams;
  private resolveExited!: (code: number) => void;
  readonly exited = new Promise<number>((resolve) => (this.resolveExited = resolve));

  constructor(readonly id: string) {}

  get events() {
    return this.q;
  }
  get pid() {
    return this.proc?.pid;
  }

  async spawn(opts: SpawnOpts): Promise<void> {
    let pty: any;
    try {
      pty = await import('node-pty');
    } catch {
      // util-linux `script` supplies a real pseudoterminal when node-pty's native
      // module is unavailable (as on the default Node 22 deployment). Coding
      // agent TUIs require a TTY; plain child-process pipes are not sufficient.
      const command = [opts.cmd ?? 'bash', ...(opts.args ?? [])].map(shellQuote).join(' ');
      this.child = spawnChild('script', ['-qefc', command, '/dev/null'], {
        cwd: opts.cwd,
        env: { ...process.env, TERM: process.env.TERM ?? 'xterm-256color' },
        stdio: ['pipe', 'pipe', 'pipe'],
      });
      this.q.push({ kind: 'status', status: 'working' });
      const output = (data: Buffer) => this.q.push({ kind: 'raw_pty', data: new Uint8Array(data) });
      this.child.stdout.on('data', output);
      this.child.stderr.on('data', output);
      this.child.on('error', (error) => this.q.push({ kind: 'error', message: error.message }));
      this.child.on('exit', (code) => {
        const exitCode = code ?? 1;
        this.q.push({ kind: 'status', status: exitCode ? 'error' : 'idle' });
        this.q.close();
        this.resolveExited(exitCode);
      });
      return;
    }
    this.proc = pty.spawn(opts.cmd ?? 'bash', opts.args ?? [], {
      name: 'xterm-color',
      cols: 100,
      rows: 30,
      cwd: opts.cwd,
      env: process.env,
    });
    this.q.push({ kind: 'status', status: 'working' });
    this.proc.onData((s: string) => this.q.push({ kind: 'raw_pty', data: Buffer.from(s, 'utf8') }));
    this.proc.onExit((e: { exitCode: number }) => {
      this.q.push({ kind: 'status', status: e.exitCode ? 'error' : 'idle' });
      this.q.close();
      this.resolveExited(e.exitCode);
    });
  }

  prompt(): Promise<string> {
    return Promise.resolve('end_turn');
  }
  sendInput(bytes: Uint8Array): void {
    const text = Buffer.from(bytes).toString('utf8');
    if (this.child) this.child.stdin.write(text);
    else this.proc?.write(text);
  }
  resize(cols: number, rows: number): void {
    try {
      this.proc?.resize(cols, rows);
    } catch {
      /* proc may have exited */
    }
  }
  respondPermission(): void {}
  interrupt(): void {
    if (this.child) this.child.kill('SIGINT');
    else this.proc?.write('\x03');
  }
  async dispose(): Promise<void> {
    if (this.child) this.child.kill();
    else this.proc?.kill();
  }
}
