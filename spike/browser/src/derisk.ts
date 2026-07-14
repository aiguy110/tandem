// Automated proof of the shared-browser theses against a real headless Chrome
// (the same CDP surface Steel exposes):
//   1. the agent drives the browser via Playwright/CDP
//   2. a screencast streams the SAME page concurrently while the agent acts
//   3. grabbing the wheel HARD-PAUSES the agent's actions
//   4. releasing resumes the paused action
//   5. the human's input (CDP Input.*) reaches the shared browser
//
// Run: npm run derisk

import { createServer } from 'node:http';
import { readFileSync } from 'node:fs';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchChrome, SharedBrowser } from './sharedBrowser.ts';

const CDP_PORT = 9333;
const HTTP_PORT = 5188;
const rule = '─'.repeat(64);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const pubDir = new URL('../public/', import.meta.url).pathname;

function servePublic(port: number) {
  return createServer((req, res) => {
    const p = req.url === '/' ? '/target.html' : req.url!.split('?')[0];
    try {
      const body = readFileSync(join(pubDir, p));
      res.writeHead(200, { 'content-type': p.endsWith('.html') ? 'text/html' : 'text/plain' });
      res.end(body);
    } catch {
      res.writeHead(404);
      res.end('not found');
    }
  }).listen(port);
}

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · shared-browser spike — de-risk');
  console.log(rule);

  const http = servePublic(HTTP_PORT);
  const dir = mkdtempSync(join(tmpdir(), 'tandem-chrome-'));
  const proc = await launchChrome(CDP_PORT, dir);
  const sb = new SharedBrowser(`http://localhost:${CDP_PORT}`);
  await sb.connect();
  console.log('\n  ▸ headless Chrome up (CDP) · connected via Playwright connectOverCDP');

  let frames = 0;
  await sb.startScreencast(() => frames++);
  console.log('  ▸ screencast started (viewer channel)');

  // 1) agent drives
  await sb.agentDo('nav', (p) => p.goto(`http://localhost:${HTTP_PORT}/target.html`, { waitUntil: 'load' }));
  const title = await sb.agentDo('title', (p) => p.locator('#title').innerText());
  const agentDrives = /shared browser/i.test(title);

  // 2) concurrent screencast while the agent acts
  const before = frames;
  await sb.agentDo('type', (p) => p.locator('#input').fill('agent typing…'));
  await sleep(1200);
  const concurrent = frames > before && frames > 3;

  // 3) grab -> hard-pause the agent
  sb.grab();
  let navDone = false;
  const paused = sb.agentDo('nav2', (p) => p.goto(`http://localhost:${HTTP_PORT}/target.html?after=1`, { waitUntil: 'load' })).then(() => {
    navDone = true;
  });
  await sleep(700);
  const heldWhileUserOwns = navDone === false;

  // 4) release -> resume
  sb.release();
  await paused;
  const resumedAfterRelease = navDone === true;

  // 5) human input reaches the shared browser (owner = user)
  sb.grab();
  const box = await sb.page.locator('#btn').boundingBox(); // introspect layout (test-only, ungated)
  await sb.userClick(box!.x + box!.width / 2, box!.y + box!.height / 2);
  await sleep(400);
  const status = await sb.page.locator('#status').innerText();
  const userInputWorks = /CLICKED BY USER/.test(status);
  sb.release();

  const checks: [string, boolean, string][] = [
    ['agent drives the shared browser (Playwright/CDP)', agentDrives, `#title = "${title}"`],
    ['screencast streams concurrently while agent acts', concurrent, `${frames} frames captured`],
    ['grab the wheel HARD-PAUSES the agent', heldWhileUserOwns, 'agent nav held while user owned the wheel'],
    ['release resumes the paused agent action', resumedAfterRelease, 'agent nav completed after release'],
    ['human input reaches the browser (CDP Input.*)', userInputWorks, `#status = "${status}"`],
  ];

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
  console.log(
    pass
      ? '  ✅ SHARED BROWSER DE-RISKED — one browser, agent-drive + human\n     screencast/input concurrently, arbitrated by a hard-pause token.'
      : '  ❌ FAILURES ABOVE',
  );
  console.log(rule + '\n');

  await sb.stopScreencast();
  await sb.close();
  proc.kill();
  http.close();
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
