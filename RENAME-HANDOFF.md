# Agent → Session rename: status and handoff

Branch: `tandem/master/galileo-412`. Working tree clean, all tests green.

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

## Done (3 of 6 layers, 3 commits)

- **b9869c4 — Layer 1, UI display strings.** "Agents" dock → "Sessions", command
  palette titles, confirm dialogs, spawn palette.
- **dd8cf3a — Layer 5, Go identifiers.** `store.Agent`→`store.Session`,
  `AgentID`→`SessionID`, `ACPSessionID`→`ExternalSessionID`,
  `Adapter.SessionID()`→`ExternalSessionID()`.
- **81911c1 — Layer 4, store schema + migration.** Tables and columns renamed;
  `migrateLegacySessionNames` migrates existing DBs.

Verify with `go test ./...` (needs `./scripts/stage-go-ui.sh` first for the
embed, or use `-tags tandem_dev` to skip it) and `cd ui && npm test`.

## Remaining (3 layers)

### Layer 3 — wire protocol (do next; this is the hard one)

Rename `agentId` → `sessionId` in the WS envelope, and `clientMessage.
ExternalSessionID`'s json tag `sessionId` → `externalSessionId`. ~66 sites in
`internal/wsserver/wsserver.go`, plus `internal/controlmcp`, `internal/
automationmcp`, `internal/browser/takeover.go`, `internal/daemon`.

Two complications:

1. **Federation.** `wsserver.go:199` and `:1254` rewrite `agentId` when proxying
   master↔slave. A renamed master talking to an un-renamed slave breaks.
2. **Compat.** Recommended: emit **both** `agentId` and `sessionId` for one
   release, accept either inbound, then drop `agentId`. Avoids lockstep deploy.

`store.AutomationWakeup.SessionID` still has tag `json:"agentId"`
(`internal/store/automation.go:76`) — it's wire, so it belongs here.

### Layer 2 — UI internals

~1,270 refs: `AgentView`, `agents` record, `panesByAgent`, ~509 `agentId`, plus
CSS class names in `styles.css` (`.agent-host-badge`, `.agent-detail`). Blocked
on Layer 3 for the wire keys. `npm run typecheck` catches nearly everything.

**Watch out:** command ids (`agent.spawn`, `agent.close`, `nav.goToAgent`) are
persisted as localStorage keymap bindings. Renaming them silently drops users'
custom keybindings — either keep the ids or write a migration.

### Layer 6 — docs

`docs/ws-protocol.md` (42 `agentId` mentions), `docs/ui.md`,
`docs/architecture.md` ("Session/Agent registry" in the diagram), `CLAUDE.md`.

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

## Not yet done

Per `CLAUDE.md`, finished work is merged to `master` in
`/home/josiah/Projects/tandem` and `./redeploy.sh` run. **That has not
happened** — deliberately, since the rename is only half landed and the wire
protocol is mid-flight. Merge once Layer 3 is in and the UI agrees with the
daemon, or the live app will break on the envelope key mismatch.
