// Auth de-risk (D15): a WS connection without the bearer token is refused with
// close code 4401; a connection presenting the token succeeds.
//
// Run: npm run derisk:auth

import { WebSocket } from 'ws';
import { makeHarness, sleep, rule, report } from './testHarness.ts';

const PORT = 7723;

function tryConnect(port: number, token?: string): Promise<{ opened: boolean; closeCode?: number }> {
  return new Promise((res) => {
    const q = token ? `?token=${token}` : '';
    const ws = new WebSocket(`ws://127.0.0.1:${port}/${q}`);
    let opened = false;
    ws.on('open', () => {
      opened = true;
    });
    ws.on('close', (code) => res({ opened, closeCode: code }));
    ws.on('error', () => {
      /* close will still fire */
    });
  });
}

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · auth de-risk (bearer token, D15)');
  console.log(rule);

  const h = await makeHarness(PORT);

  // No token → accepted then closed 4401.
  const noTok = await tryConnect(h.port);

  // Wrong token → same.
  const badTok = await tryConnect(h.port, 'not-the-token');

  // Correct token → stays open; close it ourselves afterwards.
  const goodWs = new WebSocket(`ws://127.0.0.1:${h.port}/?token=${h.token}`);
  const goodOpen = await new Promise<boolean>((res) => {
    goodWs.on('open', () => res(true));
    goodWs.on('close', () => res(false));
    goodWs.on('error', () => res(false));
  });
  await sleep(100);
  const stillOpen = goodWs.readyState === WebSocket.OPEN;
  goodWs.close();

  const checks: [string, boolean, string][] = [
    ['no-token connection refused with 4401', noTok.closeCode === 4401, `closeCode=${noTok.closeCode}`],
    ['wrong-token connection refused with 4401', badTok.closeCode === 4401, `closeCode=${badTok.closeCode}`],
    ['valid-token connection accepted + stays open', goodOpen && stillOpen, `open=${goodOpen}, stillOpen=${stillOpen}`],
  ];

  const pass = report(checks);
  await h.stop();
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
