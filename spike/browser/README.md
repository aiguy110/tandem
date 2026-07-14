# Shared-browser spike

Proves the [shared-browser](../../docs/browser.md) thesis against a real headless Chrome
(the CDP surface Steel wraps): one browser, the **agent driving via CDP** while a **human
views a screencast and injects input**, arbitrated by a **hard-pause control token**.

## Run

```bash
npm install

# automated proof (launches headless Chrome, asserts 5 checks, exits)
npm run derisk

# live demo: a viewer showing the screencast while a demo agent drives the page
npm run broker            # then open http://localhost:5178/index.html
```

`npm run derisk` expects all five ✅:

```
✅ agent drives the shared browser (Playwright/CDP)
✅ screencast streams concurrently while agent acts     (12 frames captured)
✅ grab the wheel HARD-PAUSES the agent
✅ release resumes the paused agent action
✅ human input reaches the browser (CDP Input.*)        (#status = "CLICKED BY USER")
```

## Map to production

- `sharedBrowser.ts` `launchChrome` + `connectOverCDP` → replaced by a **Steel session**
  and its **CDP endpoint** (Steel self-hosts via Docker).
- `agentDo()` gated by the token → in production the token gates the **Playwright MCP** tool
  calls Tandem mediates.
- `startScreencast` / `Input.*` → the Browser pane's screencast + input forwarding.
- The `broker.ts` demo agent loop → a real ACP agent using Playwright MCP.

Requires a system Chrome at `/usr/bin/google-chrome-stable`.
