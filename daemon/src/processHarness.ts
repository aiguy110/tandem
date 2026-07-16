// Process-level scaffolding shared by parity/restart/workspace suites.  The
// daemon is an opaque command: callers may point this at the TypeScript daemon,
// the native Go binary, or a future implementation without changing the test.

import fs from 'node:fs';
import net from 'node:net';
import { spawn, type ChildProcess } from 'node:child_process';

export type DaemonCommand = readonly [string, ...string[]];

export interface RunningDaemon {
  command: DaemonCommand;
  proc: ChildProcess;
  port: number;
  token: string;
  stdout: () => string;
  stderr: () => string;
  stop(): Promise<void>;
}

export function parseDaemonCommand(raw = process.env.TANDEM_DAEMON_CMD): DaemonCommand {
  if (!raw) throw new Error('TANDEM_DAEMON_CMD must be a JSON array containing the daemon executable and arguments');
  let value: unknown;
  try { value = JSON.parse(raw); } catch (error) { throw new Error(`invalid TANDEM_DAEMON_CMD JSON: ${error}`); }
  if (!Array.isArray(value) || value.length === 0 || value.some((part) => typeof part !== 'string' || part.length === 0)) {
    throw new Error('TANDEM_DAEMON_CMD must be a non-empty JSON string array');
  }
  return value as [string, ...string[]];
}

export async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.unref();
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      if (!address || typeof address === 'string') return reject(new Error('failed to allocate port'));
      server.close((error) => error ? reject(error) : resolve(address.port));
    });
  });
}

export async function startDaemon(options: {
  command?: DaemonCommand;
  home: string;
  port?: number;
  env?: NodeJS.ProcessEnv;
  readyTimeoutMs?: number;
}): Promise<RunningDaemon> {
  const command = options.command ?? parseDaemonCommand();
  const port = options.port ?? await freePort();
  const proc = spawn(command[0], command.slice(1), {
    env: {
      ...process.env,
      ...options.env,
      TANDEM_HOME: options.home,
      TANDEM_PORT: String(port),
      TANDEM_BIND: '127.0.0.1',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: process.platform !== 'win32',
  });
  let stdout = '';
  let stderr = '';
  const ready = new Promise<string>((resolve, reject) => {
    let settled = false;
    const finish = (fn: () => void) => { if (!settled) { settled = true; fn(); } };
    proc.stdout!.on('data', (chunk) => {
      stdout += chunk.toString();
      const match = /TANDEM_READY port=\d+ token=(\S+)/.exec(stdout);
      if (match) finish(() => resolve(match[1]));
    });
    proc.stderr!.on('data', (chunk) => { stderr += chunk.toString(); });
    proc.once('error', (error) => finish(() => reject(error)));
    proc.once('exit', (code, signal) => finish(() => reject(new Error(
      `daemon exited before ready (code=${code}, signal=${signal})\nstdout:\n${stdout}\nstderr:\n${stderr}`,
    ))));
    setTimeout(() => finish(() => reject(new Error(
      `daemon did not become ready in ${options.readyTimeoutMs ?? 15_000}ms\nstdout:\n${stdout}\nstderr:\n${stderr}`,
    ))), options.readyTimeoutMs ?? 15_000).unref();
  });
  try {
    const token = await ready;
    let stopping: Promise<void> | undefined;
    return {
      command, proc, port, token,
      stdout: () => stdout,
      stderr: () => stderr,
      stop: () => stopping ??= stopProcessTree(proc),
    };
  } catch (error) {
    await stopProcessTree(proc);
    throw error;
  }
}

async function stopProcessTree(proc: ChildProcess): Promise<void> {
  if (proc.exitCode !== null || proc.signalCode !== null) return;
  const exited = new Promise<void>((resolve) => proc.once('exit', () => resolve()));
  try {
    if (process.platform === 'win32') proc.kill('SIGTERM');
    else process.kill(-(proc.pid!), 'SIGTERM');
  } catch { proc.kill('SIGTERM'); }
  const clean = await Promise.race([exited.then(() => true), new Promise<false>((resolve) => setTimeout(() => resolve(false), 5_000))]);
  if (!clean) {
    try {
      if (process.platform === 'win32') proc.kill('SIGKILL');
      else process.kill(-(proc.pid!), 'SIGKILL');
    } catch { proc.kill('SIGKILL'); }
    await exited;
  }
}

export async function assertPortReleased(port: number): Promise<void> {
  const server = net.createServer();
  try {
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject);
      server.listen(port, '127.0.0.1', () => resolve());
    });
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
}

export function assertRemoved(paths: string[]): void {
  const leaked = paths.filter((entry) => fs.existsSync(entry));
  if (leaked.length) throw new Error(`temporary paths leaked: ${leaked.join(', ')}`);
}
