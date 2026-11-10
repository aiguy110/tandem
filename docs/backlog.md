# Backlog

Work that is understood well enough to describe but deliberately not built. Each entry
records what the feature is, why it was deferred, and what it would cost — so a future
reader can re-decide rather than re-derive.

Subsystem-local deferrals stay with their subsystem (`## Deferred` in
[`browser.md`](browser.md), [`spawn-and-workspaces.md`](spawn-and-workspaces.md)); this file
is for work that spans subsystems or was cut from a shipped feature.

## Native (OS-level) notifications while the page is suspended

**What.** Post system-level notifications — "agent-3 finished its turn", "approval needed" —
that reach the phone when the Tandem tab is not merely backgrounded but fully suspended by
the OS.

**Status: deferred, and not currently recommended.**

**Why it came up.** Scoped alongside the chat audio player (Phases 1–3), whose requirement
was "keep the frontend connected to the daemon with the phone screen off." Playing audio
keeps the page alive, so the connection holds while a clip is playing; a silence keepalive
stretches that through idle gaps. Neither is indefinite — a fully frozen page has no live
WebSocket.

**Why it was dropped.** Web Push delivers a *notification*, not a *connection*. It cannot
carry transcript events or keep the client store in sync, so it does not satisfy the
requirement that motivated it. The most it achieves is prompting the user to unlock the
phone, at which point the existing reconnect-and-replay path does the real work. It is also
partly redundant with the in-app turn notifications Tandem already has
(`turnNotifications` in `ui/src/store.ts`, surfaced in the approvals rail).

**What it would cost.**

- A service worker (Tandem currently ships none) and a push subscription endpoint on the daemon.
- A VAPID keypair generated and persisted under `$TANDEM_HOME`.
- **Outbound traffic to a third-party push relay.** Web Push is daemon → Google/Mozilla/Apple →
  device; there is no direct path. For a daemon that otherwise binds `127.0.0.1` and talks
  only to the LAN, that is a genuine change in posture and the main open question.
- On iOS, Web Push requires the site be installed to the Home Screen as a PWA; it does not
  work in a plain Safari tab.

**Cheaper alternative worth trying first.** While the page *is* alive — foreground, or
backgrounded with audio playing — the Notifications API alone can post OS-level
notifications with no service worker, no VAPID keys, and no external relay. That covers most
of the practical value and stays entirely self-hosted. Only the truly-suspended case needs
the full Web Push stack.

**Decide before building:** is routing notification metadata through Google/Apple
infrastructure acceptable for a self-hosted tool?

## Shared browser: JavaScript dialogs are invisible to the user and block the agent

**Status: mostly done (2026-10-24); the remainder below is deferred.** See *JavaScript
dialogs* and *Bounded tool calls* in `docs/browser.md`.

**What.** When a page raises a JavaScript dialog (`beforeunload`, `alert`, `confirm`,
`prompt`) in a shared browser instance, the page stalls until something answers it. The
screencast does not show native dialogs, so the user saw a frozen page. Agent tool calls
hung.

**Seen.** Repeatedly on ceac.state.gov (DS-160). Clicking a radio button or changing a select
causes an ASP.NET postback, which fires a `beforeunload` dialog with an empty message.
Navigating away from a page with typed input does the same. On 2026-10-24 a
`browser_navigate` away from such a page hit the dialog and timed out after 60 s. The next
`browser_tabs` hung, most likely on a page-title evaluation that the blocked page never
answered and that Playwright MCP does not time out. The router's per-browser call lock then
queued every later call for that browser behind it indefinitely. The human viewer's CDP calls to the
page (`Page.enable`, screencast, input) timed out at the same time.

**Done.**

- The daemon watches every page for dialogs from the moment the page appears, shows them
  in the Browser pane with the page's own Accept/Dismiss choices, and lets the human answer
  without taking control.
- Agent browser tools fail fast while a dialog is open, saying which dialog and how to
  answer it. A call that opens a dialog returns as soon as the dialog appears.
  `browser_handle_dialog` answers through the daemon even when Playwright missed the
  dialog.
- Router calls are bounded (2 min of agent-held time). A wedged Playwright MCP child is
  restarted instead of blocking the browser forever.

**Deferred.**

- **Auto-accepting `beforeunload` for agent-initiated actions.** Not adopted: it can
  discard unsaved form input the user typed in the shared view. Revisit only as an explicit
  per-instance opt-in.
- **Dialogs that predate the watcher.** A dialog already open when the daemon attaches (for
  example after a daemon restart) cannot be seen or answered over CDP. The human must close
  the tab or restart the browser.
- **Attention rail.** A pending dialog is shown only in the Browser pane, not as an
  attention item.

## Automatic secret redaction in transcripts

**Status: proposed, not started.**

**What.** Find credentials that turn up in agent tool output and redact them before they
are stored in the event log or shown in the UI. Examples: a `docker inspect` that dumps
`TS_AUTHKEY=…` and `TANDEM_JOIN_TOKEN=…`, an `env` listing, a `.env` that was `cat`ed, a
bearer token echoed by `curl -v`. Redact both values that match known shapes (`hskey-auth-…`,
`ghp_…`, `sk-…`, AWS keys, JWTs, PEM blocks) and values of high-entropy
`*_TOKEN`/`*_KEY`/`*_SECRET`/`*PASSWORD*` assignments.

**Seen.** 2026-11-10: diagnosing a federated child running in a container, an agent ran
`docker inspect … {{json .Config.Env}}`. That put the container's Tailscale auth key and
federation join token into the transcript in plain text.

**Why it is not covered today.** The secret broker ([`secrets.md`](secrets.md)) keeps
*brokered* secrets out of transcripts, but it only recognizes values a user granted through
it during the current daemon lifetime. It does nothing for credentials an agent finds on
its own through ordinary shell tools.

**Decided: redact at ingest.** The raw value is never written to SQLite, so replay,
federation relays, history export, and backups never carry it. Debugging gives up the
ability to see the original value; that is the intended trade.

**Limits to state up front.**

- **Daemon redaction alone would not stop the model from seeing the secret.** ACP agents
  run their own tools and pass the results to their model before the daemon sees a
  `tool_call_update`. Daemon redaction protects what is persisted and displayed: SQLite,
  replay, the UI, federation relays, and exported or imported history. Keeping a secret
  out of model context needs redaction inside the harness. See *omp as Tandem's native
  agent* below, which would get both halves in one change for that harness.
- **False positives hide useful output.** Hashes, commit SHAs, and checksums look
  high-entropy. Matching on the variable name *and* the value's shape, plus a per-host
  allowlist, keeps this manageable.

**Open decisions.**

- Whether a detected secret should also raise a notification suggesting rotation.
- Whether to retroactively scrub existing event logs.

## omp as Tandem's native agent, with its transcript storage format

**Status: proposed, not started.**

**What.** Ship Oh My Pi (`omp`) as Tandem's default, first-party agent instead of one
catalog entry among several, and move Tandem's transcript storage toward omp's own
session format. Then the daemon and the harness would read and write one record of a
conversation, rather than Tandem keeping a parallel event log that it rebuilds from ACP
updates. Today that log is why restore loads the session through `session/load` and then
discards the replay ([`omp-acp-plan.md`](omp-acp-plan.md) §3.3).

**Why it came up.** Secret redaction (above). A redaction step inside omp's tool-result
path, writing to storage both sides share, would keep a detected secret out of the
model's context *and* out of Tandem's stored transcript in one implementation. With a
separate harness and daemon, those are two independent features. This is the only path
found so far that covers the model-context half for any harness.

**Relationship to existing work.** The ACP integration plan
([`omp-acp-plan.md`](omp-acp-plan.md)) and the `aiguy110/oh-my-pi` fork already carry
Tandem-specific omp changes, so the delivery mechanism exists. That plan assumes ACP
remains the boundary and Tandem keeps its own log. This item is the larger step past it.

**Costs and open questions.**

- Claude, Codex, and other ACP agents still need Tandem's own event log. Either two
  storage paths coexist, or omp's format becomes the general one and other agents'
  updates are written into it. Invariant 1 in `CLAUDE.md` (the daemon owns all state)
  has to survive either way.
- How much of the storage change can go upstream, versus living permanently on the fork.
  Upstream rules ([`omp-acp-plan.md`](omp-acp-plan.md), *Upstream contribution rules*)
  require a Discord discussion before architectural changes.
- omp's session format, its stability guarantees across releases, and how it handles
  forks, compaction, and subagents all need to be checked before committing to it. This
  entry has not verified any of them.
- Federation, history import, and full-text search all read Tandem's current schema.
