// The Tandem daemon: HTTP + WS on one port, a multi-agent registry backed by
// SQLite, restoring non-closed agents on start (D11/D14/D15).
//
//   npm run daemon          # start the daemon (default 127.0.0.1:7717)
//
// Env: TANDEM_HOME, TANDEM_PORT, TANDEM_BIND, TANDEM_UI_DIR, TANDEM_PROJECT_ROOTS,
//      TANDEM_ACP_CMD (JSON array to override how ACP agents are launched).

import { loadConfig, ensureToken } from './config.ts';
import { Db } from './db.ts';
import { AgentRegistry } from './registry.ts';
import { startServer, type Server } from './server.ts';
import { makeDriver } from './browser/driver.ts';
import { BrowserBroker } from './browser/broker.ts';

async function main() {
  const config = loadConfig();
  const token = ensureToken(config.tokenPath);
  const db = new Db(config.dbPath);

  // Shared-browser broker (Phase 5, D13): per-agent lazy provisioning behind a
  // BrowserDriver (local Chromium or Steel). Nothing spins up until first use.
  const driver = makeDriver({
    driver: config.browser.driver,
    userDataRoot: config.browser.userDataRoot,
    steelBaseUrl: config.browser.steelBaseUrl,
    steelApiKey: config.browser.steelApiKey,
    steelSessionOptions: config.browser.steelSessionOptions,
  });
  const broker = new BrowserBroker(driver);
  await broker.start();

  const controlUrlHost = config.host === '0.0.0.0' ? '127.0.0.1' : config.host;
  const registry = new AgentRegistry(db, config, { broker, controlUrl: `http://${controlUrlHost}:${config.port}`, token });

  // D11: bring persisted agents back before accepting clients.
  await registry.restoreAll();

  const displayHost = config.host === '0.0.0.0' ? '127.0.0.1' : config.host;
  const bootstrapUrl = `http://${displayHost}:${config.port}/#t=${token}`;
  let server: Server | undefined;
  let shuttingDown = false;
  const shutdown = async () => {
    if (shuttingDown) return;
    shuttingDown = true;
    // Shutdown ≠ close: agents stay live in the DB so the next start restores them.
    await server?.close();
    await registry.disposeAll();
    await broker.stop();
    db.close();
    process.exit(0);
  };

  server = await startServer(registry, {
    host: config.host,
    port: config.port,
    token,
    uiDir: config.uiDir,
    bootstrapUrl,
    broker,
    onShutdownRequested: () => void shutdown(),
  });

  console.log(`tandem daemon · http+ws on ${config.host}:${config.port} · home ${config.home}`);
  console.log(`browser: driver=${config.browser.driver} mcp=${config.browser.mcpEnabled ? 'on' : 'off'}`);
  console.log(`restored ${registry.list().length} agent(s)`);
  console.log(`bootstrap: ${bootstrapUrl}`);
  // A machine-readable ready line for tooling/tests.
  console.log(`TANDEM_READY port=${config.port} token=${token}`);

  process.on('SIGINT', shutdown);
  process.on('SIGTERM', shutdown);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
