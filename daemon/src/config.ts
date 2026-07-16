// Resolves all daemon paths + knobs from env, honoring TANDEM_HOME so tests can
// point every path (db, token, worktrees) at a throwaway dir and never touch the
// real ~/.tandem. Nothing else in the daemon reads process.env for paths.

import os from 'node:os';
import path from 'node:path';
import fs from 'node:fs';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import YAML from 'yaml';

export interface AcpLaunch {
  cmd: string;
  args: string[];
  env?: Record<string, string>;
}

export interface ResumeCliLaunch {
  cmd: string;
  args: string[]; // `{sessionId}` tokens are replaced at handoff time
  startArgs?: string[];
  env?: Record<string, string>;
}

export interface AgentDefinition {
  name: string;
  acp?: AcpLaunch;
  terminal?: ResumeCliLaunch;
}
export interface AgentProfile {
  agent: string;
  name: string;
  acpArgs: string[];
  terminalArgs: string[];
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
  // (the SpawnSpec.agent selector). `default` is used
  // when a spec omits `agent`. `override`, when set (TANDEM_ACP_CMD), forces
  // EVERY agent to that one launch — the mock hook every derisk suite relies on.
  acp: {
    default: string;
    agents: Record<string, AcpLaunch>;
    override?: AcpLaunch;
  };
  resumeCli: Record<string, ResumeCliLaunch>;
  agents: Record<string, AgentDefinition>;
  profiles: Record<string, AgentProfile>;
  defaultProfile?: string;
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

type FileConfig = {
  version?: number;
  defaults?: { agent?: string; profile?: string };
  agents?: Record<string, { name?: string; acp?: { command: string; args?: string[]; env?: Record<string, string> }; terminal?: { command: string; startArgs?: string[]; resumeArgs?: string[]; env?: Record<string, string> } }>;
  profiles?: Record<string, { agent: string; name?: string; acpArgs?: string[]; terminalArgs?: string[] }>;
};

function readFileConfig(file: string, required = false): FileConfig {
  if (!fs.existsSync(file)) {
    if (required) throw new Error(`missing shipped agent catalog: ${file}`);
    return {};
  }
  try {
    const parsed = YAML.parse(fs.readFileSync(file, 'utf8')) ?? {};
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('root must be a mapping');
    return parsed as FileConfig;
  } catch (error) {
    throw new Error(`invalid ${file}: ${(error as Error).message}`);
  }
}

function expand(value: string, substitutions: Record<string, string>): string {
  return Object.entries(substitutions).reduce((result, [key, replacement]) => result.replaceAll(`{${key}}`, replacement), value);
}

function stringArray(value: unknown, key: string): string[] {
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.some((item) => typeof item !== 'string')) throw new Error(`${key} must be an array of strings`);
  return value;
}

function stringMap(value: unknown, key: string): Record<string, string> | undefined {
  if (value === undefined) return undefined;
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.values(value).some((item) => typeof item !== 'string')) {
    throw new Error(`${key} must be a mapping of string values`);
  }
  return value as Record<string, string>;
}

function command(value: unknown, key: string): string {
  if (typeof value !== 'string' || !value.trim()) throw new Error(`${key} must be a non-empty string`);
  return value;
}

function catalogFromFile(home: string): { agents: Record<string, AgentDefinition>; profiles: Record<string, AgentProfile>; defaultAgent: string; defaultProfile?: string } {
  const daemonRoot = fileURLToPath(new URL('..', import.meta.url)).replace(/[\\/]$/, '');
  const tandemRoot = path.dirname(daemonRoot);
  // The native Tandem daemon is not itself a Node executable. Keep the current
  // Node-daemon default while allowing both implementations to use an explicit
  // external Node launcher during migration.
  const substitutions = { node: resolveUserExecutable(process.env.TANDEM_NODE_CMD || process.execPath), daemonRoot, tandemRoot, home };
  // This checked-in example is also the runtime default, keeping documentation
  // and shipped behavior in one declarative source of truth.
  const shipped = readFileConfig(path.join(tandemRoot, 'config.yml.example'), true);
  const user = readFileConfig(path.join(home, 'config.yml'));
  const agents: Record<string, AgentDefinition> = {};

  const mergeAgents = (file: FileConfig) => {
    for (const [id, value] of Object.entries(file.agents ?? {})) {
      if (!value || typeof value !== 'object') throw new Error(`agents.${id} must be a mapping`);
      const previous = agents[id];
      const acp = value.acp ? {
        cmd: resolveUserExecutable(expand(command(value.acp.command, `agents.${id}.acp.command`), substitutions)),
        args: stringArray(value.acp.args, `agents.${id}.acp.args`).map((arg) => expand(arg, substitutions)),
        env: Object.fromEntries(Object.entries(stringMap(value.acp.env, `agents.${id}.acp.env`) ?? {}).map(([key, item]) => [key, expand(item, substitutions)])),
      } : previous?.acp;
      const terminal = value.terminal
        ? {
          cmd: resolveUserExecutable(expand(command(value.terminal.command, `agents.${id}.terminal.command`), substitutions)),
          args: stringArray(value.terminal.resumeArgs, `agents.${id}.terminal.resumeArgs`).map((arg) => expand(arg, substitutions)),
          startArgs: stringArray(value.terminal.startArgs, `agents.${id}.terminal.startArgs`).map((arg) => expand(arg, substitutions)),
          env: Object.fromEntries(Object.entries(stringMap(value.terminal.env, `agents.${id}.terminal.env`) ?? {}).map(([key, item]) => [key, expand(item, substitutions)])),
        }
        : previous?.terminal;
      if (!acp && !terminal) throw new Error(`agents.${id} must configure acp or terminal`);
      agents[id] = { name: value.name ?? previous?.name ?? id, acp, terminal };
    }
  };
  mergeAgents(shipped);
  mergeAgents(user);

  // Legacy environment overrides remain as a generic compatibility overlay.
  for (const [id, definition] of Object.entries(agents)) {
    const envId = id.toUpperCase().replaceAll(/[^A-Z0-9]/g, '_');
    const acpOverride = process.env[`TANDEM_ACP_CMD_${envId}`];
    if (acpOverride) definition.acp = parseLaunchEnv(acpOverride);
    const terminalOverride = process.env[`TANDEM_RESUME_CMD_${envId}`];
    if (terminalOverride) {
      const parsed = parseLaunchEnv(terminalOverride);
      definition.terminal = { ...definition.terminal, cmd: resolveUserExecutable(parsed.cmd), args: parsed.args };
    }
  }

  const profiles: Record<string, AgentProfile> = {};
  for (const [id, value] of Object.entries({ ...(shipped.profiles ?? {}), ...(user.profiles ?? {}) })) {
    if (!value || typeof value !== 'object') throw new Error(`profiles.${id} must be a mapping`);
    if (typeof value.agent !== 'string' || !value.agent) throw new Error(`profiles.${id}.agent must be a non-empty string`);
    if (!agents[value.agent]) throw new Error(`profiles.${id} references unknown agent: ${value.agent}`);
    profiles[id] = {
      agent: value.agent,
      name: value.name ?? id,
      acpArgs: stringArray(value.acpArgs, `profiles.${id}.acpArgs`),
      terminalArgs: stringArray(value.terminalArgs, `profiles.${id}.terminalArgs`),
    };
  }
  const defaultAgent = user.defaults?.agent ?? shipped.defaults?.agent;
  if (!defaultAgent) throw new Error('shipped agent catalog must configure defaults.agent');
  if (!agents[defaultAgent]) throw new Error(`defaults.agent references unknown agent: ${defaultAgent}`);
  const defaultProfile = user.defaults?.profile ?? shipped.defaults?.profile;
  if (defaultProfile && !profiles[defaultProfile]) throw new Error(`defaults.profile references unknown profile: ${defaultProfile}`);
  return { agents, profiles, defaultAgent, defaultProfile };
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
  const catalog = catalogFromFile(home);
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
      default: catalog.defaultAgent,
      agents: Object.fromEntries(Object.entries(catalog.agents).filter(([, a]) => a.acp).map(([id, a]) => [id, a.acp!])),
      // TANDEM_ACP_CMD forces every agent to this one launch — the mock hook
      // every derisk suite (and testHarness.ts) points at a fake ACP server.
      override: process.env.TANDEM_ACP_CMD ? parseLaunchEnv(process.env.TANDEM_ACP_CMD) : undefined,
    },
    resumeCli: Object.fromEntries(Object.entries(catalog.agents).filter(([, a]) => a.terminal).map(([id, a]) => [id, a.terminal!])),
    agents: catalog.agents,
    profiles: catalog.profiles,
    defaultProfile: catalog.defaultProfile,
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
