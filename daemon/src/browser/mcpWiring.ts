// Build the per-agent MCP server registrations for session/new (Phase 5, D13):
//
//   1. Playwright MCP (snapshot mode is its default) — pointed at the broker's
//      stable per-agent CDP URL via --cdp-endpoint. The agent drives the shared
//      browser through this; the broker provisions lazily on first connect.
//   2. Tandem-control MCP (stdio) — exposes browser_request_takeover(reason),
//      reaching the daemon over localhost HTTP with the bearer token.
//
// Both are declared here but SPAWNED BY THE AGENT (the ACP MCP client). Commands
// are absolute executable paths per the McpServerStdio schema.

import { createRequire } from 'node:module';
import type { BrowserBroker } from './broker.ts';
import type { McpServerSpec } from '../types.ts';

const require = createRequire(import.meta.url);

export interface BrowserWiring {
  broker: BrowserBroker;
  // Base HTTP URL of the daemon (http://host:port) for the control MCP to call.
  controlUrl: string;
  token: string;
}

const controlMcpPath = new URL('./controlMcp.mjs', import.meta.url).pathname;

function playwrightMcpCli(): string | undefined {
  // cli.js is the package's bin but isn't in its exports map, so resolve the
  // package.json and derive the path.
  try {
    const pkg = require.resolve('@playwright/mcp/package.json');
    const cli = pkg.replace(/package\.json$/, 'cli.js');
    return fsExists(cli) ? cli : undefined;
  } catch {
    return undefined;
  }
}

import fs from 'node:fs';
function fsExists(p: string): boolean {
  try {
    fs.accessSync(p);
    return true;
  } catch {
    return false;
  }
}

export function buildBrowserMcpServers(w: BrowserWiring, agentId: string): McpServerSpec[] {
  const servers: McpServerSpec[] = [];

  const cli = playwrightMcpCli();
  if (cli) {
    servers.push({
      name: 'playwright',
      command: process.execPath, // absolute node — runs the resolved cli.js
      args: [cli, '--cdp-endpoint', w.broker.endpointFor(agentId)],
      env: [],
    });
  }

  servers.push({
    name: 'tandem-control',
    command: process.execPath,
    args: [controlMcpPath],
    env: [
      { name: 'TANDEM_CONTROL_URL', value: w.controlUrl },
      { name: 'TANDEM_TOKEN', value: w.token },
      { name: 'TANDEM_AGENT_ID', value: agentId },
    ],
  });

  return servers;
}
