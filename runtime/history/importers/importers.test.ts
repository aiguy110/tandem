import assert from "node:assert/strict";
import { mkdir, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";
import { cleanText, MAX_ENTRY_TEXT } from "./common.ts";

const historyRoot = fileURLToPath(new URL("..", import.meta.url));
const runner = join(historyRoot, "runner.ts");

type RecordValue = Record<string, unknown>;

async function run(parser: string, args: string[] = [], checkpoints: unknown[] = []): Promise<RecordValue[]> {
  const child = spawn(process.execPath, [
    "--import", "tsx", runner, join(historyRoot, "importers", parser), ...args,
  ], { cwd: join(historyRoot, ".."), stdio: ["pipe", "pipe", "pipe"] });
  child.stdin.end(`${JSON.stringify({
    protocolVersion: 1, agent: parser.replace(".ts", ""), checkpoints,
  })}\n`);
  let stdout = "";
  let stderr = "";
  child.stdout.setEncoding("utf8").on("data", (v) => { stdout += v; });
  child.stderr.setEncoding("utf8").on("data", (v) => { stderr += v; });
  const code = await new Promise<number | null>((resolve) => child.on("close", resolve));
  assert.equal(code, 0, stderr);
  return stdout.trim().split("\n").filter(Boolean).map((v) => JSON.parse(v));
}

async function fixture(name: string): Promise<string> {
  const root = await import("node:fs/promises").then(({ mkdtemp }) =>
    mkdtemp(join(tmpdir(), `tandem-${name}-`)));
  return root;
}

test("normalization rejects encoded payloads and marks text truncation", () => {
  assert.equal(cleanText("data:image/png;base64,aGVsbG8="), undefined);
  assert.equal(cleanText("QUJD".repeat(200)), undefined);
  const long = cleanText("searchable ".repeat(MAX_ENTRY_TEXT));
  assert.equal(long?.text.length, MAX_ENTRY_TEXT);
  assert.equal(long?.truncated, true);
});

test("Pi imports all branch nodes, summaries, and ignores corrupt/base64 records", async () => {
  const root = await fixture("pi");
  const path = join(root, "project", "session.jsonl");
  await mkdir(join(root, "project"), { recursive: true });
  await writeFile(path, [
    JSON.stringify({ type: "session", version: 3, id: "pi-session", cwd: "/work", timestamp: "2025-01-01T00:00:00Z" }),
    JSON.stringify({ type: "message", id: "u1", parentId: null, timestamp: "2025-01-01T00:00:01Z", message: { role: "user", content: "fix the parser" } }),
    "{broken",
    JSON.stringify({ type: "message", id: "a1", parentId: "u1", message: { role: "assistant", content: "branch one" } }),
    JSON.stringify({ type: "message", id: "a2", parentId: "u1", message: { role: "assistant", content: "branch two" } }),
    JSON.stringify({ type: "compaction", id: "c1", summary: "earlier work summarized" }),
    JSON.stringify({ type: "message", id: "binary", message: { role: "user", content: "A".repeat(1024) } }),
  ].join("\n"));

  const records = await run("pi.ts", ["--root", root]);
  const begin = records.find((v) => v.type === "begin_session")!;
  assert.deepEqual(begin.session, {
    id: "pi-session", cwd: "/work", title: "fix the parser",
    createdAt: 1735689600000, updatedAt: 1735689601000, resumable: true,
    sourceMeta: { path: "project/session.jsonl", branches: "all" },
  });
  const entries = records.filter((v) => v.type === "entry").map((v) => v.entry as RecordValue);
  assert.deepEqual(entries.map((v) => v.id), ["u1", "a1", "a2", "c1"]);
  assert.equal(entries.at(-1)?.kind, "compaction");

  const end = records.find((v) => v.type === "end_session")!;
  const unchanged = await run("pi.ts", ["--root", root], [{
    importerId: "pi", importerVersion: 1, sourceKey: end.sourceKey, checkpoint: end.checkpoint,
  }]);
  assert.deepEqual(unchanged.map((v) => v.type), ["hello", "complete"]);
  assert.deepEqual((unchanged[1].sourceKeys as string[]), [path]);
});

test("Claude normalizes messages and reads only session-local spilled tool results", async () => {
  const root = await fixture("claude");
  const transcript = join(root, "projects", "-work", "claude-id.jsonl");
  const spillDir = join(root, "projects", "-work", "claude-id", "tool-results");
  await mkdir(spillDir, { recursive: true });
  await writeFile(join(spillDir, "tool.txt"), "safe local tool output");
  await writeFile(transcript, [
    JSON.stringify({ type: "user", uuid: "u", sessionId: "claude-id", cwd: "/work", timestamp: "2025-02-01", message: { role: "user", content: "find this phrase" } }),
    JSON.stringify({ type: "assistant", uuid: "a", timestamp: "2025-02-02", message: { role: "assistant", content: [{ type: "text", text: "done" }, { type: "image", data: "ignored" }] } }),
    JSON.stringify({ type: "tool_result", uuid: "t", toolResultFile: "tool.txt" }),
    JSON.stringify({ type: "tool_result", uuid: "evil", toolResultFile: "../secret" }),
    "{\"partial\":",
  ].join("\n"));
  const records = await run("claude.ts", ["--root", root]);
  assert.equal((records.find((v) => v.type === "begin_session")!.session as RecordValue).id, "claude-id");
  const entries = records.filter((v) => v.type === "entry").map((v) => (v.entry as RecordValue).text);
  assert.deepEqual(entries, ["find this phrase", "done", "safe local tool output"]);
});

test("Codex scans active and archived rollouts and handles response/event variants", async () => {
  const root = await fixture("codex");
  const active = join(root, "sessions", "2025", "01", "rollout-a.jsonl");
  const archived = join(root, "archived_sessions", "rollout-b.jsonl");
  await mkdir(join(root, "sessions", "2025", "01"), { recursive: true });
  await mkdir(join(root, "archived_sessions"), { recursive: true });
  const content = (id: string) => [
    JSON.stringify({ timestamp: "2025-03-01T00:00:00Z", type: "session_meta", payload: { id, cwd: "/repo" } }),
    JSON.stringify({ timestamp: "2025-03-01T00:00:01Z", type: "response_item", payload: { type: "message", role: "user", content: [{ type: "input_text", text: "needle" }] } }),
    JSON.stringify({ timestamp: "2025-03-01T00:00:02Z", type: "event_msg", payload: { type: "agent_message", message: "answer" } }),
    JSON.stringify({ timestamp: "2025-03-01T00:00:03Z", type: "compacted", payload: { summary: "summary" } }),
  ].join("\n");
  await writeFile(active, content("codex-active"));
  await writeFile(archived, content("codex-archived"));
  const records = await run("codex.ts", ["--root", root]);
  const sessions = records.filter((v) => v.type === "begin_session").map((v) => v.session as RecordValue);
  assert.deepEqual(sessions.map((v) => v.id), ["codex-archived", "codex-active"]);
  assert.equal((sessions[0].sourceMeta as RecordValue).archived, true);
  assert.deepEqual(records.filter((v) => v.type === "entry").slice(0, 3).map((v) =>
    ((v.entry as RecordValue).text)), ["needle", "answer", "summary"]);
});

test("OpenCode reads SQLite in read-only mode and normalizes JSON data", async (t) => {
  const root = await fixture("opencode");
  const dbPath = join(root, "opencode.db");
  const moduleName = "node:sqlite";
  const { DatabaseSync } = await import(moduleName).catch(() => ({ DatabaseSync: undefined })) as {
    DatabaseSync?: new (path: string) => {
      exec(sql: string): void;
      prepare(sql: string): { run(...args: unknown[]): void };
      close(): void;
    };
  };
  if (!DatabaseSync) return t.skip("node:sqlite unavailable");
  const db = new DatabaseSync(dbPath);
  db.exec("CREATE TABLE session (id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER)");
  db.exec("CREATE TABLE message (id TEXT, session_id TEXT, data TEXT, created_at INTEGER)");
  db.exec("CREATE TABLE part (id TEXT, message_id TEXT, session_id TEXT, data TEXT, created_at INTEGER)");
  db.prepare("INSERT INTO session VALUES (?, ?, ?, ?, ?)").run("oc-1", "/repo", "OpenCode fixture", 1000, 2000);
  db.prepare("INSERT INTO message VALUES (?, ?, ?, ?)").run("m1", "oc-1", JSON.stringify({ role: "user", content: "database needle" }), 1000);
  db.prepare("INSERT INTO part VALUES (?, ?, ?, ?, ?)").run("p1", "m1", "oc-1", JSON.stringify({ type: "text", text: "assistant detail" }), 2000);
  db.close();
  const records = await run("opencode.ts", ["--database", dbPath]);
  const begin = records.find((v) => v.type === "begin_session")!;
  assert.deepEqual(begin.session, {
    id: "oc-1", cwd: "/repo", title: "OpenCode fixture",
    createdAt: 1000000, updatedAt: 2000000, resumable: true,
    sourceMeta: { database: dbPath },
  });
  assert.deepEqual(records.filter((v) => v.type === "entry").map((v) =>
    ((v.entry as RecordValue).text)), ["database needle", "assistant detail"]);
  const end = records.find((v) => v.type === "end_session")!;
  assert.deepEqual(Object.keys(end.checkpoint as RecordValue).sort(), ["database", "wal"]);
  const unchanged = await run("opencode.ts", ["--database", dbPath], [{
    importerId: "opencode", importerVersion: 1,
    sourceKey: end.sourceKey, checkpoint: end.checkpoint,
  }]);
  assert.deepEqual(unchanged.map((v) => v.type), ["hello", "complete"]);
  assert.deepEqual((unchanged[1].sourceKeys as string[]), [end.sourceKey]);
});

test("OpenCode conservatively imports legacy session-shaped JSON", async () => {
  const root = await fixture("opencode-legacy");
  const storage = join(root, "opencode", "storage", "session");
  await mkdir(storage, { recursive: true });
  await writeFile(join(storage, "legacy.json"), JSON.stringify({
    id: "legacy-1", directory: "/old-repo", title: "Legacy",
    messages: [{ id: "old-message", role: "user", content: "legacy needle" }],
  }));
  await writeFile(join(root, "opencode", "storage", "settings.json"), JSON.stringify({
    id: "not-a-session", theme: "dark",
  }));
  const records = await run("opencode.ts", ["--root", root]);
  assert.equal((records.find((v) => v.type === "begin_session")!.session as RecordValue).id, "legacy-1");
  assert.deepEqual(records.filter((v) => v.type === "entry").map((v) =>
    ((v.entry as RecordValue).text)), ["legacy needle"]);
});
