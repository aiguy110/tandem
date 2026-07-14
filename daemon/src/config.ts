// Resolves all daemon paths + knobs from env, honoring TANDEM_HOME so tests can
// point every path (db, token, worktrees) at a throwaway dir and never touch the
// real ~/.tandem. Nothing else in the daemon reads process.env for paths.

import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import crypto from 'node:crypto';

export interface Config {
  home: string;
  dbPath: string;
  tokenPath: string;
  worktreesDir: string;
  host: string;
  port: number;
  uiDir?: string;
  projectRoots: string[];
  // Depth (in directories) to descend under each project root while scanning
  // for git repos for the spawn palette (list_dirs). 1 = root's immediate
  // children are checked (the common ~/Projects/<repo> layout).
  dirScanDepth: number;
  // How to launch an ACP agent subprocess. Default targets the real
  // claude-agent-acp; tests override via TANDEM_ACP_CMD (a JSON array).
  acpLaunch: { cmd: string; args: string[] };
  // Shared-browser subsystem (Phase 5, D13).
  browser: {
    driver: 'local' | 'steel'; // TANDEM_BROWSER_DRIVER (default local)
    userDataRoot: string; // per-agent chromium user-data dirs under TANDEM_HOME
    steelBaseUrl?: string; // STEEL_BASE_URL (required for driver=steel)
    steelApiKey?: string; // STEEL_API_KEY (optional)
    // Register the Playwright + Tandem-control MCP servers at session/new.
    // TANDEM_BROWSER_MCP=off disables it (mock/derisk suites keep it off unless
    // testing browsers); default ON for real agents.
    mcpEnabled: boolean;
  };
}

function acpLaunchFromEnv(): { cmd: string; args: string[] } {
  const raw = process.env.TANDEM_ACP_CMD;
  if (raw) {
    try {
      const arr = JSON.parse(raw);
      if (Array.isArray(arr) && arr.length) return { cmd: String(arr[0]), args: arr.slice(1).map(String) };
    } catch {
      /* fall through to string form: "cmd arg arg" */
      const parts = raw.split(/\s+/).filter(Boolean);
      if (parts.length) return { cmd: parts[0], args: parts.slice(1) };
    }
  }
  // Default: the real @agentclientprotocol/claude-agent-acp entry, run via node.
  const entry = new URL('../node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js', import.meta.url).pathname;
  return { cmd: process.execPath, args: [entry] };
}

export function loadConfig(): Config {
  const home = process.env.TANDEM_HOME || path.join(os.homedir(), '.tandem');
  fs.mkdirSync(home, { recursive: true });
  const worktreesDir = path.join(home, 'worktrees');
  const roots = (process.env.TANDEM_PROJECT_ROOTS || path.join(os.homedir(), 'Projects'))
    .split(path.delimiter)
    .filter(Boolean);
  return {
    home,
    dbPath: path.join(home, 'tandem.db'),
    tokenPath: path.join(home, 'token'),
    worktreesDir,
    host: process.env.TANDEM_BIND || '127.0.0.1',
    port: Number(process.env.TANDEM_PORT || 7717),
    uiDir: process.env.TANDEM_UI_DIR || undefined,
    projectRoots: roots,
    dirScanDepth: Number(process.env.TANDEM_DIR_SCAN_DEPTH || 1),
    acpLaunch: acpLaunchFromEnv(),
    browser: {
      driver: process.env.TANDEM_BROWSER_DRIVER === 'steel' ? 'steel' : 'local',
      userDataRoot: path.join(home, 'browser-profiles'),
      steelBaseUrl: process.env.STEEL_BASE_URL || undefined,
      steelApiKey: process.env.STEEL_API_KEY || undefined,
      // Default ON; TANDEM_BROWSER_MCP=off disables. (The mock-agent derisk suites
      // set it off; derisk:browser drives the broker directly.)
      mcpEnabled: process.env.TANDEM_BROWSER_MCP !== 'off',
    },
  };
}

// Read the bearer token, generating a fresh 32-byte hex one (mode 0600) on first
// run. D15: localhost bind + bearer token, URL-fragment bootstrap.
export function ensureToken(tokenPath: string): string {
  try {
    return fs.readFileSync(tokenPath, 'utf8').trim();
  } catch {
    const token = crypto.randomBytes(32).toString('hex');
    fs.writeFileSync(tokenPath, token, { mode: 0o600 });
    fs.chmodSync(tokenPath, 0o600); // ensure mode even if umask interfered on create
    return token;
  }
}
