import { existsSync } from "node:fs";
import { isAbsolute, join, relative, resolve } from "node:path";
import { defineHistoryImporter, type HistoryEntry } from "../sdk.ts";
import {
  argValue, entry, filenameSessionId, homePath, object, readJSONLines,
  sameCheckpoint, sourceCheckpoint, string, timestamp, walk,
} from "./common.ts";

async function rolloutPaths(root: string): Promise<Set<string>> {
  const paths = new Set<string>();
  // Current Codex indexes rollout locations in state_5.sqlite. This read-only
  // query observes WAL state but never creates/migrates or locks the database.
  const dbPath = join(root, "state_5.sqlite");
  if (existsSync(dbPath)) {
    try {
      const moduleName = "node:sqlite";
      const { DatabaseSync } = await import(moduleName) as unknown as {
        DatabaseSync: new (path: string, options: { readOnly: boolean }) => {
          prepare(sql: string): { all(): Array<Record<string, unknown>> }; close(): void;
        };
      };
      const db = new DatabaseSync(dbPath, { readOnly: true });
      try {
        const columns = db.prepare("PRAGMA table_info(threads)").all().map((v) => String(v.name));
        const column = ["rollout_path", "rolloutPath", "path"].find((v) => columns.includes(v));
        if (column) {
          for (const row of db.prepare(`SELECT "${column}" AS path FROM threads WHERE "${column}" IS NOT NULL`).all()) {
            if (typeof row.path === "string") {
              paths.add(isAbsolute(row.path) ? row.path : resolve(root, row.path));
            }
          }
        }
      } finally { db.close(); }
    } catch { /* schema/version mismatch: filesystem discovery below is authoritative fallback */ }
  }
  for (const directory of [join(root, "sessions"), join(root, "archived_sessions")]) {
    for await (const path of walk(directory, [".jsonl"])) paths.add(path);
  }
  return paths;
}

export default defineHistoryImporter({
  id: "codex",
  version: 1,
  async scan(ctx) {
    const root = argValue(ctx.args, "--root") ?? process.env.CODEX_HOME ?? homePath(".codex");
    for (const path of [...await rolloutPaths(root)].sort()) {
      const checkpoint = await sourceCheckpoint(path);
      const sourceKey = path;
      ctx.source(sourceKey);
      if (!checkpoint || sameCheckpoint(ctx.checkpoints.get(sourceKey), checkpoint)) continue;
      const records = await readJSONLines(path);
      const meta = records.map(({ value }) => object(object(value)?.payload))
        .find((payload) => payload && (payload.id || payload.session_id) && payload.cwd);
      const entries: HistoryEntry[] = [];
      for (const { value, line } of records) {
        const row = object(value);
        const payload = object(row?.payload);
        if (!row || !payload || row.type === "session_meta") continue;
        const payloadType = string(payload.type);
        const role = string(payload.role) ??
          (payloadType === "user_message" ? "user" : payloadType === "agent_message" ? "assistant" : "");
        const kind = row.type === "compacted" || payloadType?.includes("compact")
          ? "compaction" : (payloadType ?? string(row.type) ?? "event");
        const parsed = entry(
          string(payload.id) ?? `${line}`, line, role, kind,
          payload.content ?? payload.message ?? payload.text ?? payload.summary ?? payload,
          row.timestamp ?? payload.timestamp,
        );
        if (parsed) entries.push(parsed);
      }
      if (!entries.length && !meta) continue;
      const times = entries.map((v) => v.timestamp).filter((v): v is number => v !== undefined);
      await ctx.session({
        session: {
          id: string(meta?.id) ?? string(meta?.session_id) ?? filenameSessionId(path),
          cwd: string(meta?.cwd),
          title: entries.find((v) => v.role === "user")?.text.slice(0, 120),
          createdAt: timestamp(meta?.timestamp) ?? times[0],
          updatedAt: times.at(-1) ?? timestamp(meta?.timestamp),
          resumable: true,
          sourceMeta: { path: relative(root, path), archived: path.includes("archived_sessions") },
        },
        sourceKey, entries, checkpoint,
      });
    }
  },
});
