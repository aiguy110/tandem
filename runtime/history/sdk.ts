import { createInterface } from "node:readline";

export const HISTORY_PROTOCOL_VERSION = 1 as const;

export type JSONValue =
  | null
  | boolean
  | number
  | string
  | JSONValue[]
  | { [key: string]: JSONValue };

export interface ImportCheckpoint {
  importerId: string;
  importerVersion: number;
  sourceKey: string;
  checkpoint: JSONValue;
}

export interface HistoryImportRequest {
  protocolVersion: typeof HISTORY_PROTOCOL_VERSION;
  agent: string;
  checkpoints: ImportCheckpoint[];
}

export interface HistorySession {
  id: string;
  cwd?: string;
  title?: string;
  createdAt?: number;
  updatedAt?: number;
  resumable?: boolean;
  sourceMeta?: { [key: string]: JSONValue };
}

export interface HistoryEntry {
  id: string;
  ordinal: number;
  role?: string;
  kind?: string;
  timestamp?: number;
  text: string;
  truncated?: boolean;
}

export interface SessionImport {
  session: HistorySession;
  sourceKey: string;
  entries: AsyncIterable<HistoryEntry> | Iterable<HistoryEntry>;
  checkpoint: JSONValue;
}

export interface HistoryImportContext {
  agent: string;
  args: string[];
  checkpoints: ReadonlyMap<string, JSONValue>;
  /** Reports a discovered source even when its checkpoint is unchanged. */
  source(sourceKey: string): void;
  session(value: SessionImport): Promise<void>;
}

export interface HistoryImporter {
  id: string;
  version: number;
  scan(ctx: HistoryImportContext): Promise<void>;
}

export function defineHistoryImporter(importer: HistoryImporter): HistoryImporter {
  return importer;
}

function write(record: unknown): Promise<void> {
  return new Promise((resolve, reject) => {
    const line = `${JSON.stringify(record)}\n`;
    process.stdout.write(line, (error) => error ? reject(error) : resolve());
  });
}

async function readRequest(): Promise<HistoryImportRequest> {
  const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
  for await (const line of lines) {
    const request = JSON.parse(line) as HistoryImportRequest;
    if (request.protocolVersion !== HISTORY_PROTOCOL_VERSION ||
        typeof request.agent !== "string" ||
        !Array.isArray(request.checkpoints)) {
      throw new Error("invalid history import request");
    }
    lines.close();
    return request;
  }
  throw new Error("history import request was not provided");
}

/** @internal Used by Tandem's runner, not by importer modules directly. */
export async function runHistoryImporter(
  importer: HistoryImporter,
  args: string[],
): Promise<void> {
  const request = await readRequest();
  if (!importer || typeof importer.id !== "string" || !importer.id ||
      !Number.isSafeInteger(importer.version) || importer.version < 1 ||
      typeof importer.scan !== "function") {
    throw new Error("invalid history importer export");
  }
  await write({
    type: "hello",
    protocolVersion: HISTORY_PROTOCOL_VERSION,
    importer: { id: importer.id, version: importer.version },
  });
  const checkpoints = new Map(
    request.checkpoints
      .filter(({ importerId, importerVersion }) =>
        importerId === importer.id && importerVersion === importer.version)
      .map(({ sourceKey, checkpoint }) => [sourceKey, checkpoint]),
  );
  const sources = new Set<string>();
  await importer.scan({
    agent: request.agent,
    args,
    checkpoints,
    source(sourceKey) {
      if (!sourceKey) throw new Error("history source key is required");
      sources.add(sourceKey);
    },
    async session(value) {
      sources.add(value.sourceKey);
      await write({
        type: "begin_session",
        mode: "replace",
        sourceKey: value.sourceKey,
        session: value.session,
      });
      for await (const entry of value.entries) {
        await write({ type: "entry", entry });
      }
      await write({
        type: "end_session",
        sourceKey: value.sourceKey,
        checkpoint: value.checkpoint,
      });
    },
  });
  await write({ type: "complete", sourceKeys: [...sources].sort() });
}
