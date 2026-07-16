# Shared browser

Joint agent + human control of one browser **per agent**. The agent drives via Playwright
MCP over CDP; the user views and controls the same browser via a CDP screencast, arbitrated
by a **control-owner token**. Builds on D4 (CDP-screencast, Steel-based) and D13.

> **Status: production on the Go daemon.** The broker, both drivers, CDP-proxy hard-pause
> gate, MCP wiring, screencast/input over the daemon WS, and UI Browser pane live in
> `internal/browser/` + `ui/src/components/panes/BrowserPane.tsx`. The process-level browser
> tests live with the Go browser packages.

## Topology

```
        ┌───────────── Steel session (self-hosted, Docker) · per agent ──────────┐
        │                        one Chromium, CDP endpoint                        │
        └───────────────────────────────────────────────────────────────────────┘
             ▲ CDP (Playwright)                         ▲ CDP screencast + Input.*
             │                                          │
   Playwright MCP  ──▶ Tandem browser broker            Browser pane (user)
   (--cdp-endpoint = broker URL)   │ lazily provisions   grab / release the wheel
             ▲ MCP tools           │ Steel on first use
             │                     ▼
        the ACP agent        Steel session (created on demand)
```

Both the agent (Playwright over CDP) and the user (screencast + `Input.*` over CDP) are
clients of **one** browser; the token serializes them.

## Agent → browser (Playwright MCP)

- **Every agent has browser tools, provisioned lazily.** Tandem registers a Playwright MCP
  server in the agent's ACP `mcpServers` at `session/new`, pointed at a **stable per-agent
  CDP URL served by a Tandem browser broker**. The broker spins up the Steel session on the
  **first** browser tool call and proxies CDP thereafter — so no Steel session exists until a
  browser is actually used.
- Playwright MCP runs in **snapshot mode** (deterministic accessibility tree) — the agent
  acts on structure, the human on pixels; same page, two lenses.
- Alongside it, Tandem exposes a small **Tandem-control MCP** with
  `browser.request_takeover(reason)` (see Attention).
- Isolation: **one Steel session per agent** (own cookies/auth/state), matching the
  worktree-per-agent model. Agents never collide on one page.

## User → browser

- The Browser pane renders Steel's **screencast** (`Page.startScreencast`) and forwards the
  user's mouse/keyboard back via `Input.dispatchMouseEvent` / `dispatchKeyEvent`.
- Only the **focused** agent's browser streams live (bandwidth); others pause and resume on
  focus — mirrors the terminal focus rule.

## Control token & enforcement (hard-pause)

A per-browser **control owner** — `agent` or `user`. The non-owner is view-only.

- Because the Playwright MCP is **Tandem-mediated**, when `controlOwner = user` Tandem
  **holds the agent's browser tool calls** (async, like a slow tool) until control returns —
  no races, no errors, no agent cooperation required.
- When `controlOwner = agent`, the user's input events are suppressed (view-only), but the
  user can **grab** at any time, which immediately pauses the agent.

## Attention: signaling that a human is needed

Folds into the same attention/approvals rail as permissions. Two directions:

- **User-initiated grab:** *Grab the wheel* → Tandem pauses the agent → user drives →
  *Release* → the agent's next tool call resumes. The agent never had to know.
- **Agent-initiated handoff:** the agent calls `browser.request_takeover("log in to
  continue")`. Tandem (which hosts that MCP) sets the agent's status to
  **`blocked · needs you`**, adds an attention item, and shows a Browser-pane banner
  *"web-1 needs you — log in to continue. [Take the wheel]"*. When the user takes the wheel,
  completes the step, and hands back (release), the agent's blocked tool call **resolves with
  the result** and it continues.

## Lifecycle

- The Steel session is provisioned lazily and **torn down when the agent closes**.
- Browser auth/state is per-agent and **ephemeral in v1** — not restored across daemon
  restart (the agent re-navigates). Persisting browser cookies/state is deferred.

## What's real (Phase 5)

Implemented in `internal/browser/` (`driver.go`, `broker.go`, `shared_browser.go`, `mcp.go`,
`takeover.go`) and wired through the native registry and daemon; the control MCP is the
`tandem mcp-control` subcommand. UI code remains in `BrowserPane.tsx` + `browserHub.ts`.

### Driver selection (D13 amendment)

One `BrowserDriver` seam — `provision(agentId) → { cdpUrl }` / `teardown(agentId)`:

- **`LocalChromiumDriver`** (default): launches Playwright's bundled headless Chromium per
  agent with a per-agent user-data dir under `$TANDEM_HOME/browser-profiles/<agent>`.
  Used by all automated tests and wherever no Steel/Docker exists.
- **`SteelDriver`**: `POST {STEEL_BASE_URL}/v1/sessions` → the session's CDP websocket
  URL (host-normalized, see below); `POST .../{id}/release` on teardown. **Verified
  field names are isolated in one driver if a Steel build differs.

Config: `TANDEM_BROWSER_DRIVER=local|steel` (default `local`; `steel` requires
`STEEL_BASE_URL`, optional `STEEL_API_KEY`). `TANDEM_BROWSER_MCP=off` disables the MCP
registration at `session/new` (mock-agent derisk suites run with it off; default on).
The Go local driver accepts `TANDEM_CHROMIUM_EXECUTABLE`; when unset it searches ordinary
Chromium/Chrome executable names on `PATH` (and standard macOS application paths). It does
not depend on Playwright's private browser installation or `chromium.executablePath()`.

### Self-hosting Steel

Steel is the open-source headless-browser API (`ghcr.io/steel-dev/steel-browser`) that
provisions/releases per-agent Chrome sessions over a CDP endpoint. Run one with Docker:

```bash
docker run -d --name steel --shm-size=2g -p 3000:3000 -p 9223:9223 \
  -e CHROME_HEADLESS=false -e DISPLAY=:10 \
  --entrypoint /bin/sh ghcr.io/steel-dev/steel-browser:latest \
  -c 'Xvfb :10 -screen 0 1920x1080x24 -nolisten tcp & exec /app/api/entrypoint.sh'
```

- **Ports:** `3000` = REST API **and** the browser-level CDP websocket (`ws://host:3000/`);
  `9223` = Steel's CDP/debugger HTTP; UI at `http://localhost:3000/ui`.
- **`--shm-size=2g`** — Chrome exhausts the default 64 MB `/dev/shm` and dies with SIGTRAP.
- **Headful + Xvfb** — removes Chrome's explicit headless identity while retaining a virtual
  display suitable for a server. Setting `CHROME_HEADLESS=false` alone is insufficient: the
  current image contains Xvfb but does not start it, so Chrome otherwise fails with
  `Missing X server or $DISPLAY`.
- Point Tandem at it:
  ```bash
  TANDEM_BROWSER_DRIVER=steel STEEL_BASE_URL=http://localhost:3000 ./start-dev-server.sh
  ```
  (For Steel Cloud / an authed deployment, also set `STEEL_API_KEY`.)
- Run the Go browser tests with `go test ./internal/browser`.

Tandem passes optional Steel session settings from `STEEL_SESSION_OPTIONS`. For the bundled
Chromium 149 image, a practical anti-detection baseline is:

```bash
export STEEL_SESSION_OPTIONS='{
  "userAgent":"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
  "dimensions":{"width":1365,"height":768},
  "persistProfile":true
}'
```

Keep the Chrome major version aligned with the image: a mismatched UA and client-hint
fingerprint is more detectable than the default. Tandem remembers a returned `profileId`
per agent and supplies it on that agent's next Steel session. Self-hosted Steel has no
Profiles API and ignores `persistProfile`; its live session still retains cookies until the
agent is closed. Steel Cloud can additionally accept `deviceConfig`, `stealthConfig`,
`solveCaptcha`, and `useProxy` through the same JSON object. Interaction pacing is controlled
by the agent/Playwright workflow, not by Steel's session API.

**How the CDP URL is derived (why `SteelDriver` doesn't just use `/json/version`):** Steel's
`/json/version` (port 9223) advertises a *port-less* `ws://localhost/devtools/...` that
Playwright dials as `:80` and fails. So the driver uses the create response's
`websocketUrl` (`ws://0.0.0.0:3000/`) and rewrites the bind-address host to `STEEL_BASE_URL`'s
host — a dialable `ws://host:3000/`. The broker accepts that ws:// upstream directly
(`resolveBrowserWs` + the `onHttp` ws-branch synthesize the `/json/version` Playwright needs).

**Bundled-Chromium launch failures.** Steel ships its own Chromium; on some hosts (e.g.
Ubuntu 24.04) it crashes on launch (`Failed to launch the browser process` / SIGTRAP) for
reasons unrelated to Tandem. Point Steel at a known-good Chromium instead — mount one in and
set `CHROME_EXECUTABLE_PATH` (Playwright's bundled Chromium works well):

```bash
PW=$(node -e "console.log(require('playwright-core').chromium.executablePath())")
docker run -d --name steel --shm-size=2g -p 3000:3000 -p 9223:9223 \
  -v "$(dirname "$PW")":/opt/chromium:ro -e CHROME_EXECUTABLE_PATH=/opt/chromium/chrome \
  ghcr.io/steel-dev/steel-browser:latest
```

If Chrome still won't start, add `--security-opt seccomp=unconfined --security-opt apparmor=unconfined`.

### The broker: a gated CDP proxy (the design choice)

The broker runs its own localhost HTTP/WS server and serves a **stable per-agent CDP URL**
(`http://127.0.0.1:<broker>/cdp/<agentId>`) that Playwright MCP gets via `--cdp-endpoint`.
Nothing is provisioned until the **first CDP request** hits that URL (HTTP `/json/version`
or the WS upgrade); the broker then provisions via the driver, rewrites the advertised
`webSocketDebuggerUrl` to point back through itself, and **proxies CDP frames**.

The hard-pause gate lives **in this proxy**, not in front of the MCP process: while
`controlOwner = user`, CDP frames from the agent's connection are **queued** (not forwarded,
not errored) and flushed in order on release. This was chosen over fronting the MCP process
because Playwright MCP speaks CDP directly to the endpoint we hand it — the proxy is the one
layer that (a) preserves the laziness invariant (provision on first connect) and (b) can hold
the agent's actions with zero agent/MCP cooperation. The daemon's own screencast + `Input.*`
run on a **separate, ungated** CDP connection, so the human's view and input keep working
while the agent is held. (One CDP subtlety: frames must be relayed as **text**, not binary —
Chrome closes the socket otherwise.)

### Wire protocol (see ws-protocol.md)

- `browser_frame` / `browser_state` (daemon → UI), `browser_input` / `browser_control`
  (UI → daemon); `takeover_request` is a normalized `AgentEvent` on the transcript channel.
- Screencast follows the **focus rule**: only a client subscribed to the agent's `browser`
  channel streams; the UI subscribes it only while the Browser pane is open for the focused
  agent, and the daemon stops the cast when the last subscriber leaves.

### Attention (implemented)

The Tandem-control MCP (`tandem mcp-control`, over stdio) exposes
`browser_request_takeover(reason)`. Tandem supplies both this declaration and the external
`@playwright/mcp` declaration in ACP `session/new`; the ACP agent owns and cleans up both
MCP subprocesses. The configured `TANDEM_NODE_CMD` runtime launches Playwright MCP rather
than deriving Node from the Tandem executable.
It POSTs to the daemon's internal HTTP surface (bearer-token authed); the daemon emits
`{kind:'takeover_request', reqId, reason}` + `status: blocked`, and the tool call blocks until
the human **releases** the wheel, then returns and the agent resumes (`working`). The UI shows
the Browser-pane banner and an attention card in the approvals rail.

## Deferred

- **VNC/desktop fallback** for OS-level needs (native file pickers, dialogs, extensions,
  non-browser apps) — only if CDP screencast proves insufficient.
- **Shared profile / cross-agent logins** (log in once, many agents reuse).
- **Persisted browser sessions** across restart.
