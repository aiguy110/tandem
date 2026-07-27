import { basename, dirname, join, relative } from "node:path";
import { defineHistoryImporter, type HistoryEntry, type JSONValue } from "../sdk.ts";
import {
  argValue, entry, filenameSessionId, homePath, object, readJSONLines,
  readTextFileSafely, sameCheckpoint, sourceCheckpoint, string,
  timestamp, walk,
} from "./common.ts";

export default defineHistoryImporter({
  id: "claude",
  version: 1,
  async scan(ctx) {
    const configRoot = argValue(ctx.args, "--root") ??
      process.env.CLAUDE_CONFIG_DIR ?? homePath(".claude");
    const root = join(configRoot, "projects");
    for await (const path of walk(root, [".jsonl"])) {
      // Spilled tool results are .txt and therefore never mistaken for sessions.
      const transcriptCheckpoint = await sourceCheckpoint(path);
      const sourceKey = path;
      if (!transcriptCheckpoint) continue;
      const spills: Record<string, JSONValue> = {};
      const spillRoot = join(dirname(path), filenameSessionId(path), "tool-results");
      for await (const spill of walk(spillRoot, [".txt"])) {
        spills[basename(spill)] = await sourceCheckpoint(spill) ?? null;
      }
      const checkpoint = { transcript: transcriptCheckpoint, spills };
      if (sameCheckpoint(ctx.checkpoints.get(sourceKey), checkpoint)) continue;
      const records = await readJSONLines(path);
      const entries: HistoryEntry[] = [];
      let cwd: string | undefined;
      let sessionId: string | undefined;
      for (const { value, line } of records) {
        const row = object(value);
        if (!row) continue;
        cwd ??= string(row.cwd);
        sessionId ??= string(row.sessionId) ?? string(row.session_id);
        const message = object(row.message);
        const type = string(row.type) ?? "message";
        const role = string(message?.role) ?? (type === "user" || type === "assistant" ? type : "");
        let content: unknown = message?.content ?? row.content ?? row.summary;
        // Claude may replace a large tool result with a file reference. Only
        // follow a basename inside this session's own tool-results directory.
        const ref = string(row.toolResultFile) ?? string(row.tool_result_file);
        if (!content && ref && basename(ref) === ref) {
          content = await readTextFileSafely(join(spillRoot, ref));
        }
        const parsed = entry(
          string(row.uuid) ?? string(row.id) ?? `${line}`, line, role, type,
          content, row.timestamp ?? message?.timestamp,
        );
        if (parsed) entries.push(parsed);
      }
      if (!entries.length) continue;
      const times = entries.map((v) => v.timestamp).filter((v): v is number => v !== undefined);
      await ctx.session({
        session: {
          id: sessionId ?? filenameSessionId(path),
          cwd,
          title: entries.find((v) => v.role === "user")?.text.slice(0, 120),
          createdAt: times[0], updatedAt: times.at(-1), resumable: true,
          sourceMeta: { path: relative(configRoot, path) },
        },
        sourceKey, entries, checkpoint,
      });
    }
  },
});
