// Resolves all daemon paths + knobs from env, honoring TANDEM_HOME so tests can
// point every path (db, token, worktrees) at a throwaway dir and never touch the
// real ~/.tandem. Nothing else in the daemon reads process.env for paths.

import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import crypto from 'node:crypto';

export interface AcpLaunch {
  cmd: string;
  args: string[];
}

export interface ResumeCliLaunch {
  cmd: string;
  args: string[]; // `{sessionId}` tokens are replaced at handoff time
}

export interface Config {
  home: string;
  dbPath: string;
  tokenPath: string;
  worktreesDir: string;
  assetsDir: string;
  host: string;
  port: number;
  uiDir?: string;
  projectRoots: string[];
  // Depth (in directories) to descend under each project root while scanning
  // for git repos for the spawn palette (list_dirs). 1 = root's immediate
  // children are checked (the common ~/Projects/<repo> layout).
  dirScanDepth: number;
  // How to launch each supported ACP agent subprocess, keyed by agent name
  // (the SpawnSpec.agent selector — claude | codex | pi | …). `default` is used
  // when a spec omits `agent`. `override`, when set (TANDEM_ACP_CMD), forces
  // EVERY agent to that one launch — the mock hook every derisk suite relies on.
  acp: {
    default: string;
    agents: Record<string, AcpLaunch>;
    override?: AcpLaunch;
  };
  resumeCli: Record<string, ResumeCliLaunch>;
  // Shared-browser subsystem (Phase 5, D13).
  browser: {
    driver: 'local' | 'steel'; // TANDEM_BROWSER_DRIVER (default local)
    userDataRoot: string; // per-agent chromium user-data dirs under TANDEM_HOME
    steelBaseUrl?: string; // STEEL_BASE_URL (required for driver=steel)
    steelApiKey?: string; // STEEL_API_KEY (optional)
    steelSessionOptions: Record<string, unknown>; // merged into POST /v1/sessions
    // Register the Playwright + Tandem-control MCP servers at session/new.
    // TANDEM_BROWSER_MCP=off disables it (mock/derisk suites keep it off unless
    // testing browsers); default ON for real agents.
    mcpEnabled: boolean;
  };
}

// Parses a launch override from an env var, accepting either a JSON array
// (`["cmd", "arg1", ...]`) or a plain whitespace-split string ("cmd arg arg").
function parseLaunchEnv(raw: string): AcpLaunch {
  try {
    const arr = JSON.parse(raw);
    if (Array.isArray(arr) && arr.length) return { cmd: String(arr[0]), args: arr.slice(1).map(String) };
  } catch {
    /* fall through to string form */
  }
  const parts = raw.split(/\s+/).filter(Boolean);
  return { cmd: parts[0], args: parts.slice(1) };
}

// systemd user services commonly have a deliberately narrow PATH that omits
// ~/.local/bin even though interactive coding-agent installers place their
// launchers there. Resolve resume CLIs once at daemon startup so node-pty never
// depends on execvp seeing the user's shell PATH.
function resolveUserExecutable(cmd: string): string {
  if (cmd.includes(path.sep)) return cmd;
  const dirs = [
    ...(process.env.PATH ?? '').split(path.delimiter),
    path.join(os.homedir(), '.local', 'bin'),
    path.join(os.homedir(), 'bin'),
  ].filter(Boolean);
  for (const dir of [...new Set(dirs)]) {
    const candidate = path.join(dir, cmd);
    try {
      fs.accessSync(candidate, fs.constants.X_OK);
      return candidate;
    } catch {
      // Keep searching; returning the bare command below preserves the useful
      // native spawn error when the CLI truly is not installed.
    }
  }
  return cmd;
}

// Bundled ACP entry for each supported agent, run via `node <dist/index.js>`.
// Each ships as a devDependency so a fresh install always has a working
// default; a per-agent env var (TANDEM_ACP_CMD_<NAME>) can point at a
// different install (e.g. a system-wide binary) instead.
const BUNDLED_ENTRIES: Record<string, string> = {
  claude: '../node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js',
  codex: '../node_modules/@agentclientprotocol/codex-acp/dist/index.js',
  pi: '../node_modules/pi-acp/dist/index.js',
};

function acpAgentsFromEnv(): Record<string, AcpLaunch> {
  const agents: Record<string, AcpLaunch> = {};
  for (const [name, relEntry] of Object.entries(BUNDLED_ENTRIES)) {
    const envVar = `TANDEM_ACP_CMD_${name.toUpperCase()}`;
    const raw = process.env[envVar];
    if (raw) {
      agents[name] = parseLaunchEnv(raw);
      continue;
    }
    const entry = new URL(relEntry, import.meta.url).pathname;
    agents[name] = { cmd: process.execPath, args: [entry] };
  }
  return agents;
}

function resumeClisFromEnv(): Record<string, ResumeCliLaunch> {
  const defaults: Record<string, ResumeCliLaunch> = {
    claude: { cmd: 'claude', args: ['--resume', '{sessionId}'] },
    codex: { cmd: 'codex', args: ['resume', '{sessionId}'] },
    pi: { cmd: 'pi', args: ['--session', '{sessionId}'] },
  };
  for (const name of Object.keys(defaults)) {
    const raw = process.env[`TANDEM_RESUME_CMD_${name.toUpperCase()}`];
    if (raw) defaults[name] = parseLaunchEnv(raw);
    defaults[name] = { ...defaults[name], cmd: resolveUserExecutable(defaults[name].cmd) };
  }
  return defaults;
}

function parseSteelSessionOptions(raw?: string): Record<string, unknown> {
  if (!raw) return {};
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch (error) {
    throw new Error(`STEEL_SESSION_OPTIONS must be valid JSON: ${(error as Error).message}`);
  }
  if (!value || Array.isArray(value) || typeof value !== 'object') throw new Error('STEEL_SESSION_OPTIONS must be a JSON object');
  return value as Record<string, unknown>;
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
    assetsDir: path.join(home, 'assets'),
    host: process.env.TANDEM_BIND || '127.0.0.1',
    port: Number(process.env.TANDEM_PORT || 7717),
    uiDir: process.env.TANDEM_UI_DIR || undefined,
    projectRoots: roots,
    dirScanDepth: Number(process.env.TANDEM_DIR_SCAN_DEPTH || 1),
    acp: {
      default: 'claude',
      agents: acpAgentsFromEnv(),
      // TANDEM_ACP_CMD forces every agent to this one launch — the mock hook
      // every derisk suite (and testHarness.ts) points at a fake ACP server.
      override: process.env.TANDEM_ACP_CMD ? parseLaunchEnv(process.env.TANDEM_ACP_CMD) : undefined,
    },
    resumeCli: resumeClisFromEnv(),
    browser: {
      driver: process.env.TANDEM_BROWSER_DRIVER === 'steel' ? 'steel' : 'local',
      userDataRoot: path.join(home, 'browser-profiles'),
      steelBaseUrl: process.env.STEEL_BASE_URL || undefined,
      steelApiKey: process.env.STEEL_API_KEY || undefined,
      steelSessionOptions: parseSteelSessionOptions(process.env.STEEL_SESSION_OPTIONS),
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
