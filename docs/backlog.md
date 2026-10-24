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
