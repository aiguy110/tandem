import { relative } from "node:path";
import { defineHistoryImporter, type HistoryEntry, type HistorySession } from "../sdk.ts";
import {
  argValue, entry, homePath, object, readJSONLines, sameCheckpoint,
  sourceCheckpoint, stableId, string, timestamp, walk,
} from "./common.ts";

export default defineHistoryImporter({
  id: "pi",
  version: 1,
  async scan(ctx) {
    const root = argValue(ctx.args, "--root") ??
      process.env.PI_CODING_AGENT_SESSION_DIR ?? homePath(".pi", "agent", "sessions");
    for await (const path of walk(root, [".jsonl"])) {
      const checkpoint = await sourceCheckpoint(path);
      const sourceKey = path;
      if (!checkpoint || sameCheckpoint(ctx.checkpoints.get(sourceKey), checkpoint)) continue;
      const records = await readJSONLines(path);
      const header = records.map(({ value }) => object(value))
        .find((v) => v?.type === "session");
      if (!header) continue;
      const sessionId = string(header.id) ?? stableId(path);
      const entries: HistoryEntry[] = [];
      for (const { value, line } of records) {
        const row = object(value);
        if (!row || row.type === "session") continue;
        const kind = string(row.type) ?? "message";
        const message = object(row.message);
        const role = string(message?.role) ?? (kind.includes("summary") || kind === "compaction" ? "system" : "");
        const parsed = entry(
          string(row.id) ?? `${line}`, line, role, kind,
          message?.content ?? row.summary ?? row.content ?? row,
          row.timestamp ?? message?.timestamp,
        );
        if (parsed) entries.push(parsed);
      }
      const times = entries.map((v) => v.timestamp).filter((v): v is number => v !== undefined);
      const session: HistorySession = {
        id: sessionId,
        cwd: string(header.cwd),
        title: string(header.name) ?? entries.find((v) => v.role === "user")?.text.slice(0, 120),
        createdAt: timestamp(header.timestamp) ?? times[0],
        updatedAt: times.at(-1) ?? timestamp(header.timestamp),
        resumable: true,
        // Pi stores a tree in one JSONL. All branches are searchable; parent IDs
        // remain in the vendor source, while entry IDs preserve stable nodes.
        sourceMeta: { path: relative(root, path), branches: "all" },
      };
      await ctx.session({ session, sourceKey, entries, checkpoint });
    }
  },
});
