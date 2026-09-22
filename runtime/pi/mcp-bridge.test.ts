import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

// A minimal stdio MCP server: one echo tool, one failing tool, one image tool.
const FAKE_STDIO_SERVER = `
const readline = require("node:readline");
const rl = readline.createInterface({ input: process.stdin });
const send = (m) => process.stdout.write(JSON.stringify(m) + "\\n");
rl.on("line", (line) => {
  const msg = JSON.parse(line);
  if (msg.method === "initialize") {
    send({ jsonrpc: "2.0", id: msg.id, result: { protocolVersion: msg.params.protocolVersion, capabilities: { tools: {} }, serverInfo: { name: "fake", version: "1" } } });
    // Exercise server-to-client requests during the handshake.
    send({ jsonrpc: "2.0", id: "srv-1", method: "ping" });
  } else if (msg.method === "tools/list") {
    if (!msg.params.cursor) {
      send({ jsonrpc: "2.0", id: msg.id, result: { nextCursor: "page2", tools: [
        { name: "echo", description: "Echo text", inputSchema: { $schema: "http://json-schema.org/draft-07/schema#", type: "object", properties: { text: { type: "string" } }, required: ["text"] } },
      ] } });
    } else {
      send({ jsonrpc: "2.0", id: msg.id, result: { tools: [
        { name: "fail", inputSchema: { type: "object" } },
        { name: "pixel", description: "Image", inputSchema: { type: "object", properties: {} } },
        { name: "env", description: "Read env", inputSchema: { type: "object", properties: {} } },
      ] } });
    }
  } else if (msg.method === "tools/call") {
    const { name, arguments: args } = msg.params;
    if (name === "echo") send({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: "echo:" + args.text }] } });
    else if (name === "fail") send({ jsonrpc: "2.0", id: msg.id, result: { isError: true, content: [{ type: "text", text: "boom" }] } });
    else if (name === "pixel") send({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "image", data: "AAAA", mimeType: "image/png" }] } });
    else if (name === "env") send({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: process.env.FAKE_TOKEN || "" }] } });
  }
});
`;

type Registered = { name: string; label: string; description: string; parameters: any; execute: Function };

async function loadBridge(servers: unknown[]) {
  const dir = mkdtempSync(join(tmpdir(), "tandem-mcp-bridge-"));
  const file = join(dir, "servers.json");
  writeFileSync(file, JSON.stringify(servers));
  process.env.TANDEM_MCP_SERVERS_FILE = file;
  process.env.TANDEM_MCP_BRIDGE_LOG = join(dir, "bridge.log");
  const tools = new Map<string, Registered>();
  const handlers = new Map<string, Function>();
  const pi = {
    registerTool: (t: Registered) => tools.set(t.name, t),
    on: (event: string, fn: Function) => handlers.set(event, fn),
  };
  const mod = await import(`./mcp-bridge.ts?case=${Math.random()}`);
  mod.default(pi);
  return { tools, handlers, dir };
}

test("bridges stdio MCP tools into pi tools", async () => {
  const dir = mkdtempSync(join(tmpdir(), "tandem-mcp-fake-"));
  const script = join(dir, "server.cjs");
  writeFileSync(script, FAKE_STDIO_SERVER);
  const { tools, handlers } = await loadBridge([
    { name: "fake.srv", command: process.execPath, args: [script], env: [{ name: "FAKE_TOKEN", value: "tok-123" }] },
    { name: "broken", command: join(dir, "does-not-exist"), args: [], env: [] },
  ]);
  await handlers.get("session_start")!({}, {});
  try {
    assert.deepEqual([...tools.keys()].sort(), ["fake_srv_echo", "fake_srv_env", "fake_srv_fail", "fake_srv_pixel"]);
    const echo = tools.get("fake_srv_echo")!;
    assert.equal(echo.parameters.$schema, undefined);
    assert.deepEqual(echo.parameters.required, ["text"]);
    assert.deepEqual(tools.get("fake_srv_fail")!.parameters.properties, {});

    const result = await echo.execute("call-1", { text: "hi" });
    assert.deepEqual(result.content, [{ type: "text", text: "echo:hi" }]);
    const image = await tools.get("fake_srv_pixel")!.execute("call-2", {});
    assert.deepEqual(image.content, [{ type: "image", data: "AAAA", mimeType: "image/png" }]);
    const env = await tools.get("fake_srv_env")!.execute("call-3", {});
    assert.deepEqual(env.content, [{ type: "text", text: "tok-123" }]);
    await assert.rejects(tools.get("fake_srv_fail")!.execute("call-4", {}), /boom/);
  } finally {
    await handlers.get("session_shutdown")!({}, {});
  }
});

test("bridges streamable HTTP MCP tools with SSE responses and session ids", async () => {
  const seen: Array<{ method: string; session?: string; auth?: string }> = [];
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      if (req.method === "DELETE") return res.writeHead(200).end();
      const msg = JSON.parse(body);
      seen.push({ method: msg.method, session: req.headers["mcp-session-id"] as string, auth: req.headers.authorization });
      if (msg.id === undefined) return res.writeHead(202).end();
      let result: unknown;
      if (msg.method === "initialize") result = { protocolVersion: msg.params.protocolVersion, capabilities: {}, serverInfo: { name: "h", version: "1" } };
      else if (msg.method === "tools/list") result = { tools: [{ name: "add", inputSchema: { type: "object", properties: { a: { type: "number" }, b: { type: "number" } } } }] };
      else result = { content: [{ type: "text", text: String(msg.params.arguments.a + msg.params.arguments.b) }] };
      if (msg.method === "tools/call") {
        res.writeHead(200, { "content-type": "text/event-stream" });
        res.write(`event: message\ndata: ${JSON.stringify({ jsonrpc: "2.0", method: "notifications/progress", params: {} })}\n\n`);
        res.end(`event: message\ndata: ${JSON.stringify({ jsonrpc: "2.0", id: msg.id, result })}\n\n`);
      } else {
        res.writeHead(200, { "content-type": "application/json", "mcp-session-id": "sess-1" });
        res.end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result }));
      }
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  const { port } = server.address() as { port: number };
  try {
    const { tools, handlers } = await loadBridge([
      { name: "calc", type: "http", url: `http://127.0.0.1:${port}/mcp`, headers: [{ name: "Authorization", value: "Bearer x" }] },
    ]);
    await handlers.get("session_start")!({}, {});
    const result = await tools.get("calc_add")!.execute("c", { a: 2, b: 3 });
    assert.deepEqual(result.content, [{ type: "text", text: "5" }]);
    assert.deepEqual(seen.map((s) => s.method), ["initialize", "notifications/initialized", "tools/list", "tools/call"]);
    assert.equal(seen[0].session, undefined);
    assert.ok(seen.slice(1).every((s) => s.session === "sess-1" && s.auth === "Bearer x"));
    await handlers.get("session_shutdown")!({}, {});
  } finally {
    server.close();
  }
});

test("does nothing without a servers file", async () => {
  delete process.env.TANDEM_MCP_SERVERS_FILE;
  const handlers = new Map<string, Function>();
  const mod = await import(`./mcp-bridge.ts?case=${Math.random()}`);
  mod.default({ registerTool: () => assert.fail("registered a tool"), on: (e: string, fn: Function) => handlers.set(e, fn) });
  assert.equal(handlers.size, 0);
});
