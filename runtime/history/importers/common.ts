import { createHash } from "node:crypto";
import { open, readFile, readdir, stat } from "node:fs/promises";
import { homedir } from "node:os";
import { basename, extname, join } from "node:path";
import type { HistoryEntry, JSONValue } from "../sdk.ts";

export const MAX_ENTRY_TEXT = 64 * 1024;

export function homePath(...parts: string[]): string {
  return join(homedir(), ...parts);
}

export function argValue(args: string[], name: string): string | undefined {
  const at = args.indexOf(name);
  return at >= 0 ? args[at + 1] : undefined;
}

export async function* walk(root: string, extensions: string[]): AsyncGenerator<string> {
  let children;
  try {
    children = await readdir(root, { withFileTypes: true });
  } catch {
    return;
  }
  children.sort((a, b) => a.name.localeCompare(b.name));
  for (const child of children) {
    const path = join(root, child.name);
    if (child.isDirectory()) yield* walk(path, extensions);
    else if (child.isFile() && extensions.includes(extname(child.name).toLowerCase())) yield path;
  }
}

export async function sourceCheckpoint(path: string): Promise<JSONValue | undefined> {
  try {
    const info = await stat(path);
    return { size: info.size, mtimeMs: Math.trunc(info.mtimeMs) };
  } catch {
    return undefined;
  }
}

export function sameCheckpoint(a: JSONValue | undefined, b: JSONValue | undefined): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

export async function readJSONLines(path: string): Promise<Array<{ value: unknown; line: number }>> {
  const out: Array<{ value: unknown; line: number }> = [];
  let line = 0;
  try {
    const input = await open(path, "r");
    try {
      let carry = "";
      for await (const chunk of input.createReadStream({ encoding: "utf8" })) {
        carry += chunk;
        const lines = carry.split(/\r?\n/);
        carry = lines.pop() ?? "";
        for (const raw of lines) {
          line++;
          if (!raw.trim()) continue;
          try { out.push({ value: JSON.parse(raw), line }); } catch { /* one bad record is isolated */ }
        }
      }
      // A final complete JSON value is accepted; a truncated last line is ignored.
      if (carry.trim()) {
        line++;
        try { out.push({ value: JSON.parse(carry), line }); } catch { /* partial writer tail */ }
      }
    } finally {
      await input.close();
    }
  } catch {
    return out;
  }
  return out;
}

export function object(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown> : undefined;
}

export function string(value: unknown): string | undefined {
  return typeof value === "string" && value ? value : undefined;
}

export function timestamp(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isFinite(value)) {
    return value < 10_000_000_000 ? Math.trunc(value * 1000) : Math.trunc(value);
  }
  if (typeof value === "string") {
    const parsed = Date.parse(value);
    if (Number.isFinite(parsed)) return parsed;
  }
  return undefined;
}

function looksBinary(value: string): boolean {
  if (value.startsWith("data:") && value.includes(";base64,")) return true;
  if (value.includes("\0")) return true;
  // Plain prose can consist entirely of the base64 alphabet. Internal spaces
  // are a strong signal that this is text; JSON-embedded base64 is normally
  // unwrapped (explicit data URLs were handled above).
  if (/[ \t]/.test(value)) return false;
  const compact = value.replace(/\s/g, "");
  return compact.length > 512 && compact.length % 4 === 0 &&
    /^[A-Za-z0-9+/]+={0,2}$/.test(compact);
}

export function cleanText(value: string): { text: string; truncated: boolean } | undefined {
  const normalized = value.replace(/\r\n/g, "\n").trim();
  if (!normalized || looksBinary(normalized)) return undefined;
  if (normalized.length <= MAX_ENTRY_TEXT) return { text: normalized, truncated: false };
  return { text: normalized.slice(0, MAX_ENTRY_TEXT), truncated: true };
}

/** Extract human-readable strings while excluding metadata, images and encoded blobs. */
export function textFrom(value: unknown, depth = 0): string {
  if (depth > 8 || value == null) return "";
  if (typeof value === "string") {
    const normalized = value.replace(/\r\n/g, "\n").trim();
    return normalized && !looksBinary(normalized) ? normalized : "";
  }
  if (Array.isArray(value)) return value.map((v) => textFrom(v, depth + 1)).filter(Boolean).join("\n");
  const record = object(value);
  if (!record) return "";
  if (record.type === "image" || record.type === "image_url" || record.type === "input_image") return "";
  const preferred = ["text", "content", "output", "result", "message", "summary", "arguments", "input"];
  for (const key of preferred) {
    if (key in record) {
      const text = textFrom(record[key], depth + 1);
      if (text) return text;
    }
  }
  return "";
}

export function entry(
  id: string, ordinal: number, role: string, kind: string,
  value: unknown, at?: unknown,
): HistoryEntry | undefined {
  const cleaned = cleanText(textFrom(value));
  if (!cleaned) return undefined;
  return {
    id, ordinal, role, kind, timestamp: timestamp(at),
    text: cleaned.text, truncated: cleaned.truncated,
  };
}

export function stableId(...values: string[]): string {
  return createHash("sha256").update(values.join("\0")).digest("hex").slice(0, 32);
}

export async function readTextFileSafely(path: string): Promise<string | undefined> {
  try {
    const data = await readFile(path);
    if (data.includes(0)) return undefined;
    return cleanText(data.toString("utf8"))?.text;
  } catch {
    return undefined;
  }
}

export function filenameSessionId(path: string): string {
  return basename(path, extname(path));
}
