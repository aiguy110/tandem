// Configurable agent catalog/profile + direct-terminal launch regression.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { loadConfig } from './config.ts';
import { Db } from './db.ts';
import { AgentRegistry } from './registry.ts';

const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-config-'));
const previousHome = process.env.TANDEM_HOME;
const previousOverride = process.env.TANDEM_ACP_CMD;
try {
  process.env.TANDEM_HOME = home;
  delete process.env.TANDEM_ACP_CMD;
  fs.writeFileSync(path.join(home, 'config.yml'), `
defaults:
  agent: custom
  profile: custom-fast
agents:
  custom:
    name: Custom Agent
    acp:
      command: ${JSON.stringify(process.execPath)}
      args: [mock-acp.js]
      env: { ACP_CUSTOM: yes }
    terminal:
      command: ${JSON.stringify(process.execPath)}
      startArgs: ["-e", "console.log(process.env.CUSTOM_ENV + ':' + process.argv.slice(1).join(':'))"]
      resumeArgs: [resume, "{sessionId}"]
      env: { CUSTOM_ENV: configured }
profiles:
  custom-fast:
    agent: custom
    name: Custom Fast
    acpArgs: [--fast]
    terminalArgs: [profile-arg]
`);
  const config = loadConfig();
  const db = new Db(config.dbPath);
  const registry = new AgentRegistry(db, config);
  const catalog = registry.agentCatalog();
  if (catalog.defaultAgent !== 'custom' || catalog.defaultProfile !== 'custom-fast') throw new Error('configured defaults were not loaded');
  if (!catalog.agents.some((a) => a.id === 'claude') || !catalog.agents.some((a) => a.id === 'custom')) throw new Error('built-ins were not merged with custom agents');

  const session = await registry.spawn({ adapter: 'pty', workspace: { kind: 'existing', cwd: home }, terminalArgs: ['spawn-arg', '{cwd}', '{agentId}', '{agentName}'] });
  for (let i = 0; i < 40 && !session.log.fullHistory().some((e) => e.event.kind === 'raw_pty'); i++) {
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  const output = session.log.fullHistory()
    .filter((e) => e.event.kind === 'raw_pty')
    .map((e) => e.event.kind === 'raw_pty' ? Buffer.from(e.event.data).toString() : '')
    .join('');
  if (!output.includes(`configured:profile-arg:spawn-arg:${home}:${session.id}:${session.name}`)) throw new Error(`configured env/profile args and placeholders did not reach direct terminal: ${output}`);
  const persisted = db.getAgent(session.id)?.spec;
  if (!persisted?.resolvedLaunch?.terminal?.startArgs.includes('spawn-arg')) throw new Error('resolved spawn arguments were not persisted');
  await registry.disposeAll();
  db.close();
  fs.writeFileSync(path.join(home, 'config.yml'), 'agents:\n  broken:\n    terminal:\n      command: node\n      startArgs: --not-an-array\n');
  let invalid = '';
  try { loadConfig(); } catch (error) { invalid = (error as Error).message; }
  if (!invalid.includes('agents.broken.terminal.startArgs must be an array of strings')) throw new Error(`invalid config was not rejected clearly: ${invalid}`);
  console.log('PASS configurable agents, profiles, terminal args/env, and durable resolution');
} finally {
  if (previousHome === undefined) delete process.env.TANDEM_HOME; else process.env.TANDEM_HOME = previousHome;
  if (previousOverride === undefined) delete process.env.TANDEM_ACP_CMD; else process.env.TANDEM_ACP_CMD = previousOverride;
  fs.rmSync(home, { recursive: true, force: true });
}
