// Tiny interactive stand-in for `claude --resume <id>` used by deriskHandoff.
// It deliberately stays alive until stdin says `exit`, so the test can assert
// terminal control, input/output, and automatic return to ACP on process exit.

import process from 'node:process';

const sessionId = process.argv.at(-1);
process.stdout.write(`MOCK_RESUME_READY ${sessionId}\n`);
process.stdin.setEncoding('utf8');
process.stdin.on('data', (text) => {
  process.stdout.write(`MOCK_RESUME_INPUT ${text}`);
  if (text.includes('exit')) process.exit(0);
});
