import { spawnSync } from 'node:child_process';

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const nodeCommand = process.env.TANDEM_NODE_DAEMON_CMD ?? '["./node_modules/.bin/tsx","src/index.ts"]';
const goCommand = process.env.TANDEM_GO_DAEMON_CMD;
if (!goCommand) throw new Error('TANDEM_GO_DAEMON_CMD is required (JSON command array)');

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
