// Shared scaffolding for the derisk scripts: an in-process daemon over a
// throwaway TANDEM_HOME (so tests never touch the real ~/.tandem), wired to the
// mock ACP agent, plus a token-aware WS connect helper.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { WebSocket } from 'ws';
import { loadConfig, ensureToken } from './config.ts';
import { Db } from './db.ts';
import { AgentRegistry } from './registry.ts';
import { startServer, type Server } from './server.ts';
import { makeDriver } from './browser/driver.ts';
import { BrowserBroker } from './browser/broker.ts';

export const mockPath = new URL('./mock-acp-agent.mjs', import.meta.url).pathname;
export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
export const rule = '─'.repeat(64);

export interface Harness {
  port: number;
  token: string;
  home: string;
  registry: AgentRegistry;
  db: Db;
  server: Server;
  broker: BrowserBroker;
  stop(): Promise<void>;
}

/** In-process daemon on a temp home, launching ACP agents as the mock. The
 *  shared-browser broker is always wired (LocalChromiumDriver), but browser MCP
 *  registration defaults OFF so the mock-agent suites stay clean; pass
 *  `{ browserMcp: true }` to exercise it. */
export async function makeHarness(port: number, opts: { browserMcp?: boolean } = {}): Promise<Harness> {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-test-'));
  process.env.TANDEM_HOME = home;
  process.env.TANDEM_PORT = String(port);
  process.env.TANDEM_ACP_CMD = JSON.stringify([process.execPath, mockPath]);
  process.env.TANDEM_BROWSER_MCP = opts.browserMcp ? 'on' : 'off';
  process.env.TANDEM_BROWSER_DRIVER = 'local';
  const config = loadConfig();
  const token = ensureToken(config.tokenPath);
  const db = new Db(config.dbPath);
  const driver = makeDriver({ driver: 'local', userDataRoot: config.browser.userDataRoot });
  const broker = new BrowserBroker(driver);
  await broker.start();
  const registry = new AgentRegistry(db, config, { broker, controlUrl: `http://127.0.0.1:${port}`, token });
  await registry.restoreAll();
  const server = await startServer(registry, { host: '127.0.0.1', port, token, bootstrapUrl: `http://127.0.0.1:${port}/#t=${token}`, broker });
  return {
    port,
    token,
    home,
    registry,
    db,
    server,
    broker,
    stop: async () => {
      await server.close();
      await registry.disposeAll();
      await broker.stop();
      db.close();
      fs.rmSync(home, { recursive: true, force: true });
    },
  };
}

export interface Frame {
  t: string;
  agentId?: string;
  seq?: number;
  event?: any;
  transcript?: { seq: number; event: any }[];
  status?: string;
  pendingApprovals?: any[];
  dirs?: any[];
  error?: string;
  corrId?: string;
  // browser channel (Phase 5)
  dataB64?: string;
  meta?: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number };
  active?: boolean;
  controlOwner?: 'agent' | 'user';
  agents?: any[];
  catalog?: any;
}

/** Print a check table and return whether all passed (shared by all derisk scripts). */
export function report(checks: [string, boolean, string][]): boolean {
  console.log('\n' + rule);
  console.log('  RESULTS');
  console.log(rule);
  let pass = true;
  for (const [name, ok, detail] of checks) {
    pass = pass && ok;
    console.log(`  ${ok ? '✅' : '❌'}  ${name}`);
    console.log(`        ${detail}`);
  }
  console.log(rule);
  console.log(pass ? '  ✅ ALL CHECKS PASSED' : '  ❌ FAILURES ABOVE');
  console.log(rule + '\n');
  return pass;
}

/** Open an authenticated socket; caller drives subscribe/prompt themselves. */
export function open(port: number, token: string, onFrame: (f: Frame) => void): Promise<WebSocket> {
  return new Promise((res, rej) => {
    const ws = new WebSocket(`ws://127.0.0.1:${port}/?token=${token}`);
    ws.on('open', () => res(ws));
    ws.on('error', rej);
    ws.on('message', (raw) => onFrame(JSON.parse(raw.toString())));
  });
}
