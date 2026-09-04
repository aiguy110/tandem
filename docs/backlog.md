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
