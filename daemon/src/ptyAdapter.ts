// PTY adapter: the daemon owns a real pty master fd; the child (a TUI-only agent
// or the user's escape-hatch shell) never sees SIGHUP when a browser disconnects.
// node-pty is an optional dependency — spawn() fails gracefully if it's absent.

import { AsyncQueue } from './asyncQueue.ts';
import type { AgentAdapter, AgentEvent, SpawnOpts } from './types.ts';

export class PtyAdapter implements AgentAdapter {
  readonly capabilities = { structured: false, terminals: false, loadSession: false, fs: false };
  private q = new AsyncQueue<AgentEvent>();
  private proc: any;

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
      throw new Error('node-pty not installed (optional dependency). Run `npm i node-pty` to exercise the pty adapter.');
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
    });
  }

  prompt(): Promise<void> {
    return Promise.resolve();
  }
  sendInput(bytes: Uint8Array): void {
    this.proc?.write(Buffer.from(bytes).toString('utf8'));
  }
  respondPermission(): void {}
  interrupt(): void {
    this.proc?.write('\x03');
  }
  async dispose(): Promise<void> {
    this.proc?.kill();
  }
}
