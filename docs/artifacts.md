# Artifacts

An artifact is a rendered document an agent publishes for the user. The only
type so far is free-form HTML. The user sees it in the chat as a live,
scaled-down preview card, can open it on its own full-window page (and navigate
back to the chat), and can download it as a single `.html` file.

## Agent-facing MCP tools (`tandem-artifacts`)

Declared for every ACP agent (`tandem mcp-artifacts`, see
`internal/browser/mcp.go`) with `TANDEM_CONTROL_URL`, `TANDEM_TOKEN`,
`TANDEM_AGENT_ID` and `TANDEM_WORKSPACE_CWD`.

| Tool | Arguments | Result |
|---|---|---|
| `artifact_publish` | exactly one of `path` (file, absolute or workspace-relative) or `html` (string); `title?`, `filename?` | `{artifactId, type, title, filename, mimeType, size, sourcePath?, createdAt, status: "published", note}` |
| `artifact_docs` | `guide?` | Markdown guidance (`internal/artifactsmcp/docs.go`): rendering environment, sandbox limits, design advice, and the list of guides. With `guide`, a task playbook from `internal/artifactsmcp/guides.go` |

Guides:

- `paper-explainer` — converting/summarizing a white paper, spec, or plan as a
  "scrollytelling" explainer: one figure (often a sequence diagram) pinned at
  the top of the view, whose elements are highlighted, annotated, added, or
  animated by a pure `setStep(n)` as the step text below scrolls past.

The MCP subprocess runs on the agent's host, so it reads a `path` argument
itself and POSTs the content to `POST /internal/artifacts/publish` with the
agent's session ID. The daemon never reads agent-supplied paths. Both inputs
produce the same thing: a stored document plus a download.

## Storage and transcript

- The document is stored with `assets.Store.PutHTML`: the same content-addressed
  layout and per-session authorization as prompt images, so `artifactId` is the
  SHA-256 of the bytes and identical content dedupes. Limit: 10 MB, UTF-8.
  `assets.Store.Put` (the upload path) still accepts only images.
- The daemon appends an `artifact` event to the session log:
  `{kind, artifactId, type: "html", title, filename, mimeType, size, sourcePath?, createdAt}`.
  It replays like any transcript row, so artifacts survive reconnects.
- The UI fetches the bytes from `GET /api/agents/<session>/assets/<artifactId>`
  (bearer token). For a federated session the parent fetches them from the
  owning host over the tunnel (`get_asset`), exactly as for images. Non-image
  assets are served with `Content-Security-Policy: sandbox` and
  `Content-Disposition: attachment` so they can never execute on the daemon's
  origin.

## UI

- `ui/src/artifacts.ts` — route helpers, a small immutable-content cache, and
  the iframe sandbox flags.
- `ui/src/components/ArtifactView.tsx` — `ArtifactCard` (transcript preview: the
  page rendered at 1280×800 and CSS-scaled to the card, inert, lazily loaded
  when scrolled near) and `ArtifactPage` (full window).
- The full page is the hash route `#/artifact/<sessionId>/<artifactId>`, pushed
  onto browser history so Back (button, Esc, or the browser's own) returns to
  the chat. A hash route keeps the relative-base UI bundle loadable on reload.
  The page overlays the still-mounted app, so the chat is exactly as left.
- Artifacts render via `srcdoc` in a sandboxed iframe without
  `allow-same-origin`: scripts run with an opaque origin and cannot reach the
  bearer token in localStorage, cookies, or the app DOM. The preview allows
  only scripts; the full page also allows forms, modals, popups and downloads.

## Gallery

"Artifacts Gallery" in the command palette (`artifacts.gallery`) opens
`ui/src/components/ArtifactsGallery.tsx`: every artifact from every readable
host (`list_artifacts`, see `docs/ws-protocol.md`), newest first, as lazily
loaded preview tiles with a fuzzy title/session filter (Enter opens the top
match). It reads the `events` table through the `events_kind_ts` index, so
closed sessions' artifacts are included. An artifact opens on the usual full
page over the gallery; Back returns to it. The page takes its title and
filename from the gallery entry when the session's transcript is not loaded.
