import { existsSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { defineHistoryImporter, type HistoryEntry, type HistorySession } from "../sdk.ts";
import {
  argValue, entry, homePath, object, readJSONLines, sameCheckpoint,
  sourceCheckpoint, string, timestamp, walk,
} from "./common.ts";

type Row = Record<string, unknown>;
type Database = {
  prepare(sql: string): { all(...args: unknown[]): Row[] };
  close(): void;
};
class UnsupportedDatabaseError extends Error {}

function json(value: unknown): unknown {
  if (typeof value !== "string") return value;
  try { return JSON.parse(value); } catch { return value; }
}

async function importDatabase(ctx: Parameters<Parameters<typeof defineHistoryImporter>[0]["scan"]>[0], dbPath: string) {
  // Main-file mtime alone is insufficient in WAL mode: recent commits can
  // exist only in opencode.db-wal until checkpointed.
  const checkpoint = {
    database: await sourceCheckpoint(dbPath) ?? null,
    wal: await sourceCheckpoint(`${dbPath}-wal`) ?? null,
  };
  let db: Database;
  try {
    const moduleName = "node:sqlite";
    const { DatabaseSync } = await import(moduleName) as unknown as {
      DatabaseSync: new (path: string, options: { readOnly: boolean }) => Database;
    };
    db = new DatabaseSync(dbPath, { readOnly: true });
  } catch (error) {
    throw new UnsupportedDatabaseError("cannot open OpenCode database", { cause: error });
  }
  try {
    let sessions: Row[];
    try {
      sessions = db.prepare("SELECT * FROM session ORDER BY id").all();
    } catch (error) {
      throw new UnsupportedDatabaseError("unsupported OpenCode database schema", { cause: error });
    }
    for (const row of sessions) {
      const id = string(row.id);
      if (!id) continue;
      // A database contains many independently committed sessions. Giving
      // each one a source key prevents an interrupted scan from checkpointing
      // later, not-yet-emitted sessions by accident.
      const sourceKey = `${dbPath}#session:${encodeURIComponent(id)}`;
      if (sameCheckpoint(ctx.checkpoints.get(sourceKey), checkpoint)) continue;
      let messages: Row[] = [];
      let parts: Row[] = [];
      try { messages = db.prepare("SELECT * FROM message WHERE session_id = ? ORDER BY id").all(id); } catch { /* old schema */ }
      try { parts = db.prepare("SELECT * FROM part WHERE session_id = ? ORDER BY id").all(id); } catch { /* old schema */ }
      const entries: HistoryEntry[] = [];
      let ordinal = 0;
      for (const source of [...messages, ...parts]) {
        ordinal++;
        const data = object(json(source.data)) ?? source;
        const parsed = entry(
          string(source.id) ?? `${ordinal}`, ordinal,
          string(data.role) ?? string(source.role) ?? "",
          string(data.type) ?? (parts.includes(source) ? "part" : "message"),
          data.content ?? data.text ?? data, source.time_created ?? source.created_at ?? data.timestamp,
        );
        if (parsed) entries.push(parsed);
      }
      entries.sort((a, b) => (a.timestamp ?? a.ordinal) - (b.timestamp ?? b.ordinal));
      entries.forEach((v, i) => { v.ordinal = i + 1; });
      const session: HistorySession = {
        id, cwd: string(row.directory), title: string(row.title),
        createdAt: timestamp(row.time_created ?? row.created_at),
        updatedAt: timestamp(row.time_updated ?? row.updated_at),
        resumable: true, sourceMeta: { database: dbPath },
      };
      await ctx.session({ session, sourceKey, entries, checkpoint });
    }
  } finally { db.close(); }
}

async function importLegacy(ctx: Parameters<Parameters<typeof defineHistoryImporter>[0]["scan"]>[0], root: string) {
  // Pre-SQLite OpenCode stored JSON objects in a directory tree. Parse
  // session-shaped files defensively; messages embedded in the object are
  // indexed, while uncorrelated cache/config JSON is ignored.
  for await (const path of walk(root, [".json", ".jsonl"])) {
    const checkpoint = await sourceCheckpoint(path);
    if (!checkpoint || sameCheckpoint(ctx.checkpoints.get(path), checkpoint)) continue;
    const records = path.endsWith(".jsonl") ? (await readJSONLines(path)).map((v) => v.value) :
      await import("node:fs/promises").then(async ({ readFile }) => {
        try { return [JSON.parse(await readFile(path, "utf8"))]; } catch { return []; }
      });
    for (const raw of records) {
      const row = object(raw);
      const id = string(row?.id);
      if (!row || !id || !(row.directory || row.messages || row.title)) continue;
      const values = Array.isArray(row.messages) ? row.messages : [];
      const entries = values.map((value, i) => {
        const msg = object(value);
        return entry(string(msg?.id) ?? `${i + 1}`, i + 1, string(msg?.role) ?? "",
          string(msg?.type) ?? "message", msg?.content ?? msg, msg?.timestamp);
      }).filter((v): v is HistoryEntry => Boolean(v));
      await ctx.session({
        session: {
          id, cwd: string(row.directory), title: string(row.title),
          createdAt: timestamp(row.time_created ?? row.createdAt),
          updatedAt: timestamp(row.time_updated ?? row.updatedAt),
          resumable: true, sourceMeta: { path: relative(root, path), legacy: true },
        },
        sourceKey: path, entries, checkpoint,
      });
    }
  }
}

export default defineHistoryImporter({
  id: "opencode",
  version: 1,
  async scan(ctx) {
    const explicit = argValue(ctx.args, "--database") ?? process.env.OPENCODE_DB;
    const platformRoot = process.platform === "win32" && process.env.LOCALAPPDATA
      ? process.env.LOCALAPPDATA
      : process.platform === "darwin"
        ? homePath("Library", "Application Support")
        : homePath(".local", "share");
    const dataRoot = argValue(ctx.args, "--root") ?? process.env.XDG_DATA_HOME ?? platformRoot;
    const dbPath = explicit ?? join(dataRoot, "opencode", "opencode.db");
    if (existsSync(dbPath)) {
      try {
        await importDatabase(ctx, dbPath);
        return;
      } catch (error) {
        if (!(error instanceof UnsupportedDatabaseError)) throw error;
        // Pre-SQLite stores may coexist with an unreadable/unknown database.
      }
    }
    await importLegacy(ctx, join(explicit ? dirname(explicit) : dataRoot, "opencode", "storage"));
  },
});
