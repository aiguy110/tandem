# Agent → Session rename: complete

Branch: `tandem/master/galileo-412`.

## Goal

Make "session" the consistent name for the thing in the left dock, all the way
through the app and the code. The win is disambiguation: **"agent" currently
means two things** — the dock row, and the harness (`claude`/`codex`/`pi`) behind
it. After the rename, "agent" means *only* the harness.

Three senses must stay separate. This is the whole point; get it wrong and the
rename makes things worse:

| Sense | Name | Example |
|---|---|---|
| Tandem's own session (the dock row) | `sessionId` / `SessionID` | `sessions.id` |
| The upstream agent's own session | `externalSessionId` / `ExternalSessionID` | Claude's UUID, used for resume |
| The harness | `agent` | `Spec.Agent`, `CatalogAgent` |

## Completed

- **b9869c4 — Layer 1, UI display strings.** "Agents" dock → "Sessions", command
  palette titles, confirm dialogs, spawn palette.
- **dd8cf3a — Layer 5, Go identifiers.** `store.Agent`→`store.Session`,
  `AgentID`→`SessionID`, `ACPSessionID`→`ExternalSessionID`,
  `Adapter.SessionID()`→`ExternalSessionID()`.
- **81911c1 — Layer 4, store schema + migration.** Tables and columns renamed;
  `migrateLegacySessionNames` migrates existing DBs.
- **Layer 3 — wire protocol.** Canonical envelope identity is `sessionId` and
  upstream resume identity is `externalSessionId`. The daemon accepts legacy
  `agentId` and emits it beside `sessionId` for one release; federation relays
  translate both generations.
- **Layer 2 — UI internals.** The live dock collection is `sessions`, its view
  types and pane state use session names, and browser storage migrates old
  agent-named keys by reading them as fallbacks.
- **Layer 6 — docs.** The WebSocket and UI documentation record the distinct
  Tandem and upstream session identities and the compatibility window.

Verify with `go test ./...` (needs `./scripts/stage-go-ui.sh` first for the
embed, or use `-tags tandem_dev` to skip it) and `cd ui && npm test`.

## Deliberate exceptions — do not "fix" these

- **ACP protocol fields.** `internal/acp/`, `internal/acpadapter/` use
  `json:"sessionId"` as the ACP wire format. Renaming breaks protocol
  compliance.
- **CDP/browser session ids.** `internal/browser/shared_browser.go`,
  `driver.go`. A different domain entirely.
- **Config template tokens** `{agentId}`, `{agentName}`, `{sessionId}` in
  `expand()` (`registry.go:707`). A documented user-config surface
  (`TANDEM_RESUME_CMD_*`, `config.yml`); renaming breaks existing user configs.
- **Harness-sense strings** kept in the UI: the `label="Agent"` detail row,
  "Agent profile", "Agent default", "Configured agents without ACP session
  listing", "Agent has control" (agent vs. human peer in the browser pane).
- **`historyimport`'s `agentID`** was always a harness id; renamed to `agent`,
  not to a session id.

## Delivery

After the final verification pass, commit this completed rename, merge it to
`master`, and run `./redeploy.sh` from `/home/josiah/Projects/tandem`.
