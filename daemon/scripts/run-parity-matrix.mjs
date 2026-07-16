import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const nodeCommand = process.env.TANDEM_NODE_DAEMON_CMD ?? '["./node_modules/.bin/tsx","src/index.ts"]';
const goCommand = process.env.TANDEM_GO_DAEMON_CMD;
if (!goCommand) throw new Error('TANDEM_GO_DAEMON_CMD is required (JSON command array)');
const tempBefore = new Set(fs.readdirSync(os.tmpdir()).filter((name) => name.startsWith('tandem-')));

function run(args, env = {}) {
  const result = spawnSync(npm, args, { stdio: 'inherit', env: { ...process.env, ...env } });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
}

run(['run', 'contract:test']);
run(['run', 'derisk:all'], { TANDEM_DAEMON_CMD: nodeCommand });
run(['run', 'derisk:browser-internal']);
run(['run', 'derisk:all'], { TANDEM_DAEMON_CMD: goCommand });
run(['run', 'derisk:parity'], { TANDEM_NODE_DAEMON_CMD: nodeCommand, TANDEM_GO_DAEMON_CMD: goCommand });

const leaked = fs.readdirSync(os.tmpdir()).filter((name) => name.startsWith('tandem-') && !tempBefore.has(name));
if (leaked.length) throw new Error(`parity matrix leaked temporary paths: ${leaked.join(', ')}`);
console.log('matrix cleanup passed: no new temporary homes, projects, or worktrees');
