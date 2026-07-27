# Bundled history importers

These importers are first-party examples of Tandem's vendor-neutral history
protocol. They only discover and normalize transcript data. Tandem owns the
database, FTS index, ranking, and resume policy.

All importers:

- isolate malformed JSONL records and ignore an incomplete writer tail;
- exclude images, data URLs, likely base64/binary payloads;
- cap each normalized text entry at 64 KiB;
- use source size/mtime checkpoints to skip unchanged files;
- emit vendor session IDs unchanged where available; and
- never emit a command to execute.

Supported roots and overrides:

| Importer | Default | Environment override | Test/operator argument |
| --- | --- | --- | --- |
| Claude | `~/.claude/projects` | `CLAUDE_CONFIG_DIR` | `--root` (config root) |
| Codex | `~/.codex` | `CODEX_HOME` | `--root` |
| Pi | `~/.pi/agent/sessions` | `PI_CODING_AGENT_SESSION_DIR` | `--root` |
| OpenCode | platform data root | `OPENCODE_DB`, `XDG_DATA_HOME` | `--database`, `--root` |

Pi's JSONL is a tree rather than a single linear chat. The importer indexes all
nodes, including nodes on abandoned branches, because an old branch can contain
the only occurrence of a useful search term. The normalized entry keeps Pi's
stable node ID, and `sourceMeta.branches` advertises this policy. A later UI can
label branch-only hits; selecting a hit still resumes the vendor session, not an
individual node.

Codex uses `state_5.sqlite` as a discovery aid and scans active and archived
rollout directories as a fallback. OpenCode opens its database read-only and
includes both the database and WAL file in its checkpoint; pre-SQLite
session-shaped JSON files have a conservative fallback. Claude only follows a
spilled tool-result basename inside the matching session's `tool-results`
directory.
