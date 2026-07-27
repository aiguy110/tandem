# Transcript history importers

Tandem's history importer boundary lets an agent definition normalize an
external tool's transcripts without giving that code access to Tandem's
database. The TypeScript process reads one request from stdin and emits a
versioned NDJSON stream on stdout. Tandem validates and commits each complete
session, updates its opaque source checkpoint in the same transaction, and
maintains the full-text index.

History import is deliberately separate from resume. An importer emits an
opaque session ID and a `resumable` flag, never a command. The agent's
`history.resume` setting (`auto`, `acp`, or `terminal`) records the policy that
the resume catalog will use.

## Configuration

```yaml
agents:
  acme:
    name: Acme
    terminal:
      command: acme
      resumeArgs: [resume, "{sessionId}"]
    history:
      parser: "{home}/importers/acme.ts"
      args: [--include-tool-output]
      env:
        ACME_HISTORY_ROOT: "{home}/.acme/history"
      resume: terminal
      enabled: true
```

`parser` must resolve to an absolute path. It and every argument/environment
value support `{home}`, `{runtimeRoot}`, `{tandemRoot}`, and `{node}`
substitutions. `enabled` defaults to true and `resume` defaults to `auto`.

The process is launched directly, never through a shell. It receives a small
allowlist of discovery-related environment variables (`HOME`, `PATH`,
platform temporary-directory variables, and XDG config/data/state roots) plus
the configured `history.env` overlay. Environment values are redacted from
Tandem's configuration diagnostics.

An importer is trusted local extension code, not a security sandbox: it runs
with the daemon user's filesystem permissions. Only configure parser scripts
you trust. The restricted inherited environment, protocol validation, and
resource limits reduce accidental exposure and contain failures, but do not
prevent a malicious parser from reading local files.

## Importer API

Importers run through Tandem's pinned `tsx` dependency, so they do not depend
on experimental Node type stripping. The runtime supplies the
`@tandem/history-importer` module:

```ts
import { defineHistoryImporter } from "@tandem/history-importer";

export default defineHistoryImporter({
  id: "acme",
  version: 1,

  async scan(ctx) {
    for await (const source of discoverSessions(ctx.args)) {
      // Report every discovered source, including checkpoint-unchanged ones.
      // This lets Tandem distinguish an unchanged transcript from a deletion.
      ctx.source(source.path);
      const previous = ctx.checkpoints.get(source.path);
      if (source.isUnchanged(previous)) continue;

      await ctx.session({
        sourceKey: source.path,
        session: {
          id: source.sessionId,
          cwd: source.cwd,
          title: source.title,
          createdAt: source.createdAt,
          updatedAt: source.updatedAt,
          resumable: true,
          sourceMeta: { formatVersion: source.version },
        },
        entries: source.entries(),
        checkpoint: source.nextCheckpoint(),
      });
    }
  },
});
```

Importer IDs are non-empty stable strings. Versions are positive integers.
Changing the version hides checkpoints written by older versions from the
importer's `ctx.checkpoints` map, providing explicit and safe re-index
invalidation. Checkpoints are arbitrary JSON values interpreted only by the
importer.

Session and entry IDs are opaque vendor IDs. Entry text must be non-empty.
Timestamps are Unix milliseconds. `sourceKey` identifies the physical source
for incremental import and need not equal the resumable session ID.

## Wire protocol

The SDK handles framing, but the protocol is intentionally small and
documented for diagnostics:

```json
{"type":"hello","protocolVersion":1,"importer":{"id":"acme","version":1}}
{"type":"begin_session","mode":"replace","sourceKey":"...","session":{"id":"..."}}
{"type":"entry","entry":{"id":"...","ordinal":1,"role":"user","kind":"message","text":"..."}}
{"type":"end_session","sourceKey":"...","checkpoint":{"offset":1234}}
{"type":"complete","sourceKeys":["..."]}
```

Only replacement mode is supported initially. Tandem buffers one framed
session and commits it only after a valid matching `end_session`; malformed or
interrupted output leaves the last indexed version and checkpoint untouched.
The final source inventory is applied only after a successful process exit.
Missing sources are marked and retained for a seven-day grace period before
purge; failed scans never perform deletion reconciliation. An importer version
change suppresses old checkpoints and safely rebuilds the current generation.
Different agents' importer failures are isolated and recorded in
`history_import_runs`, while per-source checkpoints and their latest errors
live in `history_import_state`.

The runner enforces a two-minute default timeout, a 2 MiB NDJSON-line limit,
a 256 MiB total-output limit, 100,000 entries per session, and a bounded stderr
diagnostic. The daemon schedules a nonblocking startup scan, refreshes stale
indexes when the Resume surface is opened, and runs a low-frequency periodic
scan. Per-agent singleflight prevents overlap; `refresh_history` supports
explicit refresh or checkpoint-free reindex, and `history_status` reports the
latest run. Search/list requests always return the existing index without
waiting for this work. Callers invoke `historyimport.Runner.Import` or `ImportAll`;
standalone deployments first call `runtimeinstall.EnsureHistory` to stage the
embedded version-matched SDK/runner/importers and install `tsx`.
