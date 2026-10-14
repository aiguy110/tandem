import { existsSync } from "node:fs";
import { dirname, relative } from "node:path";
import { defineHistoryImporter, type HistoryEntry, type HistorySession } from "../sdk.ts";
import {
  argValue, entry, homePath, object, readJSONLines, sameCheckpoint,
  sourceCheckpoint, stableId, string, timestamp, walk,
} from "./common.ts";

export interface PiSessionImporterOptions {
  id: string;
  // Environment variable that relocates the session store, if the harness has one.
  rootEnv?: string;
  defaultRoot: string;
  // Skip transcripts nested in a `<session>/` artifact directory next to
  // `<session>.jsonl`; oh-my-pi writes its subagents' sessions there.
  skipNestedSessions?: boolean;
}

// Pi's JSONL session tree is shared by harnesses built on its runtime
// (oh-my-pi), so the scanner is parameterized by store location.
export function piSessionImporter(options: PiSessionImporterOptions) {
  return defineHistoryImporter({
    id: options.id,
    version: 1,
    async scan(ctx) {
      const root = argValue(ctx.args, "--root") ??
        (options.rootEnv ? process.env[options.rootEnv] : undefined) ?? options.defaultRoot;
      for await (const path of walk(root, [".jsonl"])) {
        if (options.skipNestedSessions && existsSync(`${dirname(path)}.jsonl`)) continue;
        const checkpoint = await sourceCheckpoint(path);
        const sourceKey = path;
        ctx.source(sourceKey);
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
          title: string(header.name) ?? string(header.title) ??
            entries.find((v) => v.role === "user")?.text.slice(0, 120),
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
}

export default piSessionImporter({
  id: "pi",
  rootEnv: "PI_CODING_AGENT_SESSION_DIR",
  defaultRoot: homePath(".pi", "agent", "sessions"),
});
