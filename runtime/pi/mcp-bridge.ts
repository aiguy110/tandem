// Tandem MCP bridge for pi.
//
// pi has no MCP client, and pi-acp accepts ACP `mcpServers` without wiring
// them into pi. Tandem therefore launches pi through `tandem-pi`, which loads
// this extension. The daemon writes the session's resolved MCP servers (the
// same list it sends in ACP session/new) to TANDEM_MCP_SERVERS_FILE; this
// extension connects to each server when the pi session starts and registers
// every MCP tool as a first-class pi tool named `<server>_<tool>`.
//
// The extension is deliberately dependency-free (node builtins only) so the
// daemon can stage it from its embedded copy without an npm install. It
// speaks MCP over stdio (newline-delimited JSON-RPC) and streamable HTTP.
//
// pi-acp discards pi's stderr, so diagnostics are also appended as JSON lines
// to TANDEM_MCP_BRIDGE_LOG when set. Never write to stdout: it carries pi's
// RPC protocol.

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { appendFileSync, mkdirSync, readFileSync } from "node:fs";
import { dirname } from "node:path";

type EnvVariable = { name: string; value: string };

type ServerConfig = {
  name: string;
  type?: string;
  command?: string;
  args?: string[];
  env?: EnvVariable[];
  url?: string;
  headers?: EnvVariable[];
};

type JsonRpcMessage = {
  jsonrpc: "2.0";
  id?: number | string | null;
  method?: string;
  params?: any;
  result?: any;
  error?: { code: number; message: string; data?: unknown };
};

type McpTool = { name: string; title?: string; description?: string; inputSchema?: Record<string, unknown> };

const PROTOCOL_VERSION = "2025-06-18";
const CLIENT_INFO = { name: "tandem-pi-mcp-bridge", version: "1.0.0" };
const CONNECT_TIMEOUT_MS = Number(process.env.TANDEM_MCP_BRIDGE_TIMEOUT_MS) || 30_000;
const MAX_TOOL_NAME = 64;

function log(level: "info" | "warn" | "error", msg: string, fields: Record<string, unknown> = {}) {
  const line = JSON.stringify({ time: new Date().toISOString(), level, msg, pid: process.pid, ...fields });
  process.stderr.write(line + "\n");
  const path = process.env.TANDEM_MCP_BRIDGE_LOG;
  if (!path) return;
  try {
    mkdirSync(dirname(path), { recursive: true });
    appendFileSync(path, line + "\n", { mode: 0o600 });
  } catch {
    // Logging must never break the agent.
  }
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function withTimeout<T>(promise: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} timed out after ${ms}ms`)), ms);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

interface Transport {
  request(method: string, params: unknown, signal?: AbortSignal): Promise<any>;
  notify(method: string, params?: unknown): Promise<void>;
  close(): void;
  readonly alive: boolean;
}

class StdioTransport implements Transport {
  private child: ChildProcessWithoutNullStreams;
  private buffer = "";
  private nextId = 1;
  private pending = new Map<number, { resolve: (v: any) => void; reject: (e: Error) => void }>();
  private exited = false;
  private closing = false;

  constructor(private server: ServerConfig) {
    const env: NodeJS.ProcessEnv = { ...process.env };
    for (const v of server.env ?? []) env[v.name] = v.value;
    this.child = spawn(server.command!, server.args ?? [], { env, stdio: ["pipe", "pipe", "pipe"] });
    this.child.stdout.setEncoding("utf8");
    this.child.stdout.on("data", (chunk: string) => this.onData(chunk));
    this.child.stderr.setEncoding("utf8");
    this.child.stderr.on("data", (chunk: string) => {
      const text = chunk.trim();
      if (text) log("info", "mcp server stderr", { server: server.name, stderr: text.slice(0, 2000) });
    });
    this.child.on("error", (err) => this.fail(new Error(`spawn ${server.command}: ${err.message}`)));
    this.child.on("exit", (code, signal) => {
      log(this.closing ? "info" : "warn", "mcp server exited", { server: server.name, code, signal });
      this.fail(new Error(`MCP server ${server.name} exited (code=${code}, signal=${signal})`));
    });
  }

  get alive() {
    return !this.exited;
  }

  private fail(err: Error) {
    this.exited = true;
    for (const p of this.pending.values()) p.reject(err);
    this.pending.clear();
  }

  private onData(chunk: string) {
    this.buffer += chunk;
    let newline: number;
    while ((newline = this.buffer.indexOf("\n")) >= 0) {
      const line = this.buffer.slice(0, newline).trim();
      this.buffer = this.buffer.slice(newline + 1);
      if (!line) continue;
      let msg: JsonRpcMessage;
      try {
        msg = JSON.parse(line);
      } catch {
        log("warn", "mcp server wrote non-JSON to stdout", { server: this.server.name, line: line.slice(0, 500) });
        continue;
      }
      this.onMessage(msg);
    }
  }

  private onMessage(msg: JsonRpcMessage) {
    if (msg.method && msg.id !== undefined && msg.id !== null) {
      // Server-to-client request. The bridge advertises no client
      // capabilities, so only ping is meaningful.
      const reply: JsonRpcMessage =
        msg.method === "ping"
          ? { jsonrpc: "2.0", id: msg.id, result: {} }
          : { jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: `Method not found: ${msg.method}` } };
      this.write(reply);
      return;
    }
    if (msg.method) return; // notification (logging, list_changed, progress)
    const id = typeof msg.id === "number" ? msg.id : Number(msg.id);
    const pending = this.pending.get(id);
    if (!pending) return;
    this.pending.delete(id);
    if (msg.error) pending.reject(new Error(`MCP error ${msg.error.code}: ${msg.error.message}`));
    else pending.resolve(msg.result);
  }

  private write(msg: JsonRpcMessage) {
    if (this.exited) throw new Error(`MCP server ${this.server.name} is not running`);
    this.child.stdin.write(JSON.stringify(msg) + "\n");
  }

  request(method: string, params: unknown, signal?: AbortSignal): Promise<any> {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      if (signal?.aborted) return reject(new Error("aborted"));
      const onAbort = () => {
        this.pending.delete(id);
        void this.notify("notifications/cancelled", { requestId: id, reason: "aborted by pi" }).catch(() => {});
        reject(new Error("aborted"));
      };
      signal?.addEventListener("abort", onAbort, { once: true });
      this.pending.set(id, {
        resolve: (v) => {
          signal?.removeEventListener("abort", onAbort);
          resolve(v);
        },
        reject: (e) => {
          signal?.removeEventListener("abort", onAbort);
          reject(e);
        },
      });
      try {
        this.write({ jsonrpc: "2.0", id, method, params });
      } catch (err) {
        this.pending.delete(id);
        reject(err as Error);
      }
    });
  }

  async notify(method: string, params?: unknown) {
    this.write({ jsonrpc: "2.0", method, ...(params === undefined ? {} : { params }) });
  }

  close() {
    this.closing = true;
    if (this.exited) return;
    this.child.stdin.end();
    this.child.kill("SIGTERM");
    const child = this.child;
    setTimeout(() => {
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    }, 2000).unref();
  }
}

class HttpTransport implements Transport {
  private nextId = 1;
  private sessionId: string | undefined;
  private protocolVersion: string | undefined;
  private closed = false;

  constructor(private server: ServerConfig) {}

  get alive() {
    return !this.closed;
  }

  private headers(): Record<string, string> {
    const h: Record<string, string> = {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
    };
    for (const v of this.server.headers ?? []) h[v.name] = v.value;
    if (this.sessionId) h["mcp-session-id"] = this.sessionId;
    if (this.protocolVersion) h["mcp-protocol-version"] = this.protocolVersion;
    return h;
  }

  private async post(msg: JsonRpcMessage, signal?: AbortSignal): Promise<Response> {
    const res = await fetch(this.server.url!, { method: "POST", headers: this.headers(), body: JSON.stringify(msg), signal });
    const session = res.headers.get("mcp-session-id");
    if (session) this.sessionId = session;
    if (!res.ok && res.status !== 202) {
      const body = await res.text().catch(() => "");
      throw new Error(`MCP HTTP ${res.status} from ${this.server.name}: ${body.slice(0, 500)}`);
    }
    return res;
  }

  async request(method: string, params: unknown, signal?: AbortSignal): Promise<any> {
    const id = this.nextId++;
    const res = await this.post({ jsonrpc: "2.0", id, method, params }, signal);
    const type = res.headers.get("content-type") ?? "";
    let reply: JsonRpcMessage | undefined;
    if (type.includes("text/event-stream")) {
      reply = await this.readEventStream(res, id);
    } else {
      const body = await res.json();
      reply = (Array.isArray(body) ? body : [body]).find((m: JsonRpcMessage) => m.id === id);
    }
    if (!reply) throw new Error(`MCP server ${this.server.name} returned no response to ${method}`);
    if (reply.error) throw new Error(`MCP error ${reply.error.code}: ${reply.error.message}`);
    if (method === "initialize") this.protocolVersion = reply.result?.protocolVersion;
    return reply.result;
  }

  private async readEventStream(res: Response, id: number): Promise<JsonRpcMessage | undefined> {
    if (!res.body) return undefined;
    const decoder = new TextDecoder();
    let buffer = "";
    for await (const chunk of res.body as unknown as AsyncIterable<Uint8Array>) {
      buffer += decoder.decode(chunk, { stream: true }).replace(/\r\n/g, "\n");
      let boundary: number;
      while ((boundary = buffer.indexOf("\n\n")) >= 0) {
        const event = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const data = event
          .split("\n")
          .filter((l) => l.startsWith("data:"))
          .map((l) => l.slice(5).replace(/^ /, ""))
          .join("\n");
        if (!data) continue;
        try {
          const msg = JSON.parse(data) as JsonRpcMessage;
          if (msg.id === id && !msg.method) return msg;
        } catch {
          // ignore malformed events
        }
      }
    }
    return undefined;
  }

  async notify(method: string, params?: unknown) {
    await this.post({ jsonrpc: "2.0", method, ...(params === undefined ? {} : { params }) });
  }

  close() {
    this.closed = true;
    if (!this.sessionId) return;
    void fetch(this.server.url!, { method: "DELETE", headers: this.headers() }).catch(() => {});
  }
}

class McpConnection {
  private transport: Transport | undefined;
  private connecting: Promise<Transport> | undefined;

  constructor(readonly server: ServerConfig) {}

  private async open(): Promise<Transport> {
    const kind = (this.server.type ?? "stdio").toLowerCase();
    let transport: Transport;
    if (kind === "http") transport = new HttpTransport(this.server);
    else if (kind === "stdio" || kind === "") transport = new StdioTransport(this.server);
    else throw new Error(`unsupported MCP transport ${JSON.stringify(this.server.type)}`);
    try {
      await transport.request("initialize", {
        protocolVersion: PROTOCOL_VERSION,
        capabilities: {},
        clientInfo: CLIENT_INFO,
      });
      await transport.notify("notifications/initialized");
    } catch (err) {
      transport.close();
      throw err;
    }
    return transport;
  }

  async connect(): Promise<Transport> {
    if (this.transport?.alive) return this.transport;
    if (!this.connecting) {
      this.connecting = this.open()
        .then((t) => (this.transport = t))
        .finally(() => (this.connecting = undefined));
    }
    return this.connecting;
  }

  async listTools(): Promise<McpTool[]> {
    const transport = await this.connect();
    const tools: McpTool[] = [];
    let cursor: string | undefined;
    do {
      const result = await transport.request("tools/list", cursor ? { cursor } : {});
      tools.push(...(result?.tools ?? []));
      cursor = result?.nextCursor || undefined;
    } while (cursor);
    return tools;
  }

  async callTool(name: string, args: unknown, signal?: AbortSignal): Promise<any> {
    // Reconnects transparently if the server process died since startup.
    const transport = await this.connect();
    return transport.request("tools/call", { name, arguments: args ?? {} }, signal);
  }

  close() {
    this.transport?.close();
    this.transport = undefined;
  }
}

function toolName(server: string, tool: string): string {
  return `${server}_${tool}`.replace(/[^A-Za-z0-9_-]/g, "_").slice(0, MAX_TOOL_NAME);
}

function toolParameters(schema: Record<string, unknown> | undefined): Record<string, unknown> {
  const params: Record<string, unknown> = schema && typeof schema === "object" ? { ...schema } : {};
  delete params.$schema;
  if (params.type !== "object") params.type = "object";
  if (!params.properties) params.properties = {};
  return params;
}

function toPiContent(result: any): Array<{ type: "text"; text: string } | { type: "image"; data: string; mimeType: string }> {
  const out: Array<{ type: "text"; text: string } | { type: "image"; data: string; mimeType: string }> = [];
  for (const item of result?.content ?? []) {
    switch (item?.type) {
      case "text":
        out.push({ type: "text", text: String(item.text ?? "") });
        break;
      case "image":
        out.push({ type: "image", data: item.data, mimeType: item.mimeType });
        break;
      case "resource":
        if (typeof item.resource?.text === "string") out.push({ type: "text", text: item.resource.text });
        else out.push({ type: "text", text: `[binary resource ${item.resource?.uri ?? ""} (${item.resource?.mimeType ?? "unknown type"})]` });
        break;
      case "resource_link":
        out.push({ type: "text", text: `[resource ${item.name ?? ""}: ${item.uri ?? ""}]` });
        break;
      default:
        out.push({ type: "text", text: `[unsupported MCP content type ${JSON.stringify(item?.type)}]` });
    }
  }
  if (out.length === 0 && result?.structuredContent !== undefined) {
    out.push({ type: "text", text: JSON.stringify(result.structuredContent) });
  }
  if (out.length === 0) out.push({ type: "text", text: "(no output)" });
  return out;
}

function loadServers(): ServerConfig[] {
  const path = process.env.TANDEM_MCP_SERVERS_FILE;
  if (!path) return [];
  try {
    const parsed = JSON.parse(readFileSync(path, "utf8"));
    if (!Array.isArray(parsed)) throw new Error("expected a JSON array");
    return parsed.filter((s) => s && typeof s.name === "string");
  } catch (err) {
    log("error", "failed to read MCP servers file", { path, error: errorMessage(err) });
    return [];
  }
}

export default function tandemMcpBridge(pi: any) {
  const servers = loadServers();
  if (servers.length === 0) return;
  const connections = servers.map((s) => new McpConnection(s));
  const registered = new Set<string>();
  let started = false;

  pi.on("session_start", async () => {
    if (started) return;
    started = true;
    const began = Date.now();
    log("info", "connecting MCP servers", { servers: servers.map((s) => s.name) });
    await Promise.all(
      connections.map(async (conn) => {
        const name = conn.server.name;
        try {
          const tools = await withTimeout(conn.listTools(), CONNECT_TIMEOUT_MS, `MCP server ${name} startup`);
          let count = 0;
          for (const tool of tools) {
            const piName = toolName(name, tool.name);
            if (registered.has(piName)) {
              log("warn", "skipping MCP tool with colliding pi name", { server: name, tool: tool.name, piName });
              continue;
            }
            registered.add(piName);
            count++;
            pi.registerTool({
              name: piName,
              label: `${name}: ${tool.title ?? tool.name}`,
              description: tool.description ?? `MCP tool ${tool.name} from ${name}`,
              parameters: toolParameters(tool.inputSchema),
              async execute(_toolCallId: string, params: unknown, signal?: AbortSignal) {
                const t0 = Date.now();
                let result: any;
                try {
                  result = await conn.callTool(tool.name, params, signal);
                } catch (err) {
                  log("warn", "MCP tool call failed", { server: name, tool: tool.name, ms: Date.now() - t0, error: errorMessage(err) });
                  throw err;
                }
                const content = toPiContent(result);
                if (result?.isError) {
                  const text = content.filter((c) => c.type === "text").map((c) => (c as { text: string }).text).join("\n");
                  log("info", "MCP tool returned error", { server: name, tool: tool.name, ms: Date.now() - t0 });
                  throw new Error(text || `MCP tool ${tool.name} failed`);
                }
                return { content, details: { server: name, tool: tool.name, structuredContent: result?.structuredContent } };
              },
            });
          }
          log("info", "registered MCP tools", { server: name, tools: count, ms: Date.now() - began });
        } catch (err) {
          conn.close();
          log("error", "MCP server unavailable", { server: name, ms: Date.now() - began, error: errorMessage(err) });
        }
      }),
    );
  });

  const closeAll = () => {
    for (const conn of connections) conn.close();
  };
  pi.on("session_shutdown", async () => closeAll());
  process.once("exit", closeAll);
}
