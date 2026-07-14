// WorkspaceFs — the daemon's filesystem sandbox choke point (docs/agent-adapter.md,
// docs/architecture.md). Services ACP `fs/read_text_file` / `fs/write_text_file`
// on an agent's behalf, but ONLY inside that agent's workspace cwd.
//
// Containment is a realpath check: the resolved target — and, for a not-yet-
// existing file, its nearest existing ancestor — must live inside the realpath'd
// workspace root. This defeats both `../` traversal and symlink escapes. A path
// that escapes is rejected with a PathEscapeError, which the adapter turns into a
// JSON-RPC error (invalid params) rather than touching the filesystem.

import fs from 'node:fs';
import path from 'node:path';
import type { WorkspaceFs as IWorkspaceFs } from './types.ts';

export class PathEscapeError extends Error {
  constructor(requested: string, root: string) {
    super(`path escapes workspace: ${requested} is not inside ${root}`);
    this.name = 'PathEscapeError';
  }
}

export class WorkspaceFs implements IWorkspaceFs {
  private root: string;

  constructor(rootDir: string) {
    // Realpath the root once so symlinked workspace roots compare correctly.
    this.root = fs.existsSync(rootDir) ? fs.realpathSync(rootDir) : path.resolve(rootDir);
  }

  // Resolve `p` (absolute or relative to the root) and prove it stays inside the
  // workspace. Returns the absolute, contained path or throws PathEscapeError.
  private contain(p: string): string {
    const abs = path.resolve(this.root, p);
    // Lexical check on the requested path (catches `../` before any fs access).
    const relAbs = path.relative(this.root, abs);
    if (relAbs === '..' || relAbs.startsWith('..' + path.sep) || path.isAbsolute(relAbs)) {
      throw new PathEscapeError(p, this.root);
    }
    // Realpath check on the nearest existing ancestor (catches symlink escapes,
    // e.g. a symlink inside the workspace pointing out of it).
    let probe = abs;
    while (probe !== path.dirname(probe) && !fs.existsSync(probe)) probe = path.dirname(probe);
    if (fs.existsSync(probe)) {
      const realProbe = fs.realpathSync(probe);
      const relReal = path.relative(this.root, realProbe);
      if (relReal === '..' || relReal.startsWith('..' + path.sep) || path.isAbsolute(relReal)) {
        throw new PathEscapeError(p, this.root);
      }
    }
    return abs;
  }

  async readTextFile(p: string, opts?: { line?: number; limit?: number }): Promise<string> {
    const abs = this.contain(p);
    const text = await fs.promises.readFile(abs, 'utf8');
    // ACP fs/read_text_file: optional 1-based `line` start and `limit` line count.
    if (opts?.line == null && opts?.limit == null) return text;
    const lines = text.split('\n');
    const start = opts?.line != null ? Math.max(0, opts.line - 1) : 0;
    const end = opts?.limit != null ? start + opts.limit : lines.length;
    return lines.slice(start, end).join('\n');
  }

  async writeTextFile(p: string, text: string): Promise<void> {
    const abs = this.contain(p);
    await fs.promises.mkdir(path.dirname(abs), { recursive: true });
    await fs.promises.writeFile(abs, text, 'utf8');
  }
}
