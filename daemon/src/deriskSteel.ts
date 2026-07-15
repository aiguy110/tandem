// Steel-driver de-risk (D13). Exercises the REAL SteelDriver + BrowserBroker
// against a live self-hosted Steel (`ghcr.io/steel-dev/steel-browser`), driving
// the agent side through the broker's gated CDP proxy exactly as Playwright MCP
// would — the same path deriskBrowser proves with LocalChromiumDriver, but with
// Steel as the browser provider.
//
// Steel isn't bundled with the repo (it needs Docker), so this suite is OPT-IN:
// it SKIPS (and passes) unless a reachable Steel is configured. Point it at one
// with STEEL_BASE_URL (default http://localhost:3000; optional STEEL_API_KEY).
// See docs/browser.md › "Self-hosting Steel" for the one-command Docker setup.
//
// Checks:
//   a) laziness: constructing the broker provisions NO Steel session; the first
//      CDP hit creates exactly one.
//   b) the agent drives a page via the broker (connectOverCDP through the proxy →
//      onHttp ws-branch → upstream ws proxy to Steel's CDP endpoint).
//   c) provision created a LIVE session in Steel (verified via Steel's REST API).
//   d) the daemon's own SharedBrowser (screencast/input connection) connects to
//      the same Steel session (resolveBrowserWs + connectOverCDP path).
//   e) teardown RELEASES the Steel session (verified via REST) and goes cold.
//
// Run: STEEL_BASE_URL=http://localhost:3000 npm run derisk:steel

import { chromium } from 'playwright-core';
import { SteelDriver } from './browser/driver.ts';
import { BrowserBroker } from './browser/broker.ts';
import { sleep, rule, report } from './testHarness.ts';

const BASE = (process.env.STEEL_BASE_URL || 'http://localhost:3000').replace(/\/$/, '');
const API_KEY = process.env.STEEL_API_KEY || undefined;

function steelHeaders(): Record<string, string> {
  const h: Record<string, string> = { 'content-type': 'application/json' };
  if (API_KEY) h['steel-api-key'] = API_KEY;
  return h;
}

async function steelReachable(): Promise<boolean> {
  try {
    const r = await fetch(`${BASE}/v1/sessions`, { headers: steelHeaders(), signal: AbortSignal.timeout(2500) });
    return r.ok;
  } catch {
    return false;
  }
}

// The live sessions Steel currently reports (status === 'live').
async function liveSessionIds(): Promise<string[]> {
  try {
    const r = await fetch(`${BASE}/v1/sessions`, { headers: steelHeaders() });
    const body = (await r.json()) as { sessions?: Array<{ id: string; status: string }> } | Array<{ id: string; status: string }>;
    const list = Array.isArray(body) ? body : (body.sessions ?? []); // Steel wraps in { sessions: [...] }
    return list.filter((s) => s.status === 'live').map((s) => s.id);
  } catch {
    return [];
  }
}

async function sessionStatus(id: string): Promise<string | undefined> {
  try {
    const r = await fetch(`${BASE}/v1/sessions/${id}`, { headers: steelHeaders() });
    if (!r.ok) return undefined;
    return ((await r.json()) as { status?: string }).status;
  } catch {
    return undefined;
  }
}

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · Steel-driver de-risk (D13 — SteelDriver + broker + gate)');
  console.log(rule);

  if (!(await steelReachable())) {
    console.log(`  ⏭  SKIP: no reachable Steel at ${BASE}`);
    console.log('     Start one with Docker (see docs/browser.md › Self-hosting Steel), then');
    console.log('     re-run: STEEL_BASE_URL=http://localhost:3000 npm run derisk:steel');
    console.log(rule + '\n');
    process.exit(0);
  }
  console.log(`  Steel: ${BASE}${API_KEY ? ' (api key set)' : ''}\n`);

  const checks: [string, boolean, string][] = [];
  const driver = new SteelDriver({ baseUrl: BASE, apiKey: API_KEY });
  const broker = new BrowserBroker(driver);
  await broker.start();
  const id = 'web-1';

  const before = new Set(await liveSessionIds());

  // ============ (a) laziness ============
  const coldAtStart = !driver.isProvisioned(id);

  // ============ (b) agent drives a page via the broker ============
  const brokerUrl = broker.endpointFor(id);
  const agentBrowser = await chromium.connectOverCDP(brokerUrl); // == Playwright MCP path
  const ctx = agentBrowser.contexts()[0] ?? (await agentBrowser.newContext());
  const page = ctx.pages()[0] ?? (await ctx.newPage());
  await page.goto('data:text/html,<title>tandem-steel</title><h1 id="h">shared browser via Steel</h1>', { waitUntil: 'load' });
  const heading = await page.locator('#h').innerText().catch(() => '');
  const agentDrives = /shared browser via steel/i.test(heading);
  const provisioned = driver.isProvisioned(id);
  checks.push(['(a) laziness: no Steel session until first CDP hit', coldAtStart, `coldAtStart=${coldAtStart}`]);
  checks.push(['(b) agent drives a page through the broker proxy (connectOverCDP)', agentDrives && provisioned, `#h="${heading}" provisioned=${provisioned}`]);

  // ============ (c) provision created a live Steel session ============
  await sleep(200);
  const after = await liveSessionIds();
  const newLive = after.filter((s) => !before.has(s));
  const createdLive = newLive.length >= 1;
  const newSessionId = newLive[0];
  checks.push(['(c) provision created a LIVE session in Steel (REST)', createdLive, `newLive=[${newLive.join(',')}] (was ${before.size})`]);

  // ============ (d) daemon SharedBrowser connects to the same session ============
  const shared = await broker.sharedBrowser(id);
  const sharedOk = !!shared.page && shared.isConnected();
  const sharedTitle = sharedOk ? await shared.page!.title().catch(() => '') : '';
  checks.push(['(d) daemon SharedBrowser (screencast conn) connects to Steel', sharedOk, `connected=${sharedOk} title="${sharedTitle}"`]);

  // ============ (e) teardown releases the Steel session ============
  await agentBrowser.close().catch(() => {});
  await broker.teardown(id);
  await sleep(300);
  const coldAfter = !driver.isProvisioned(id);
  const releasedStatus = newSessionId ? await sessionStatus(newSessionId) : 'unknown';
  const released = coldAfter && (releasedStatus === 'released' || releasedStatus === undefined);
  checks.push(['(e) teardown releases the Steel session + goes cold', released, `cold=${coldAfter} sessionStatus=${releasedStatus}`]);

  const pass = report(checks);
  await broker.stop();
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
