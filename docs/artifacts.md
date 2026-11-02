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
| `artifact_publish` | exactly one of `path` (file, absolute or workspace-relative) or `html` (string); `title?`, `filename?`; `artifactId?` to add a revision to an existing artifact, `baseRevision?` to refuse the update if a newer revision exists | the revision: `{artifactId, revision, revisionNumber, type, title, filename, mimeType, size, contentSha256, sessionId, sourcePath?, createdAt, status: "published", note}` |
| `artifact_list` | `query?`, `limit?` | `{artifacts: [revision + sessionName]}`: latest revision of every artifact on this host, newest first |
| `artifact_revisions` | `artifactId` | `{artifactId, revisions}` newest first |
| `artifact_read` | `artifactId`, `revision?` (default latest), `path?` | `{revision, content}`; with `path` the document is written to that workspace file and `content` omitted |
| `artifact_docs` | `guide?` | Markdown guidance (`internal/artifactsmcp/docs.go`): rendering environment, sandbox limits, design advice, and the list of guides. With `guide`, a task playbook from `internal/artifactsmcp/guides.go` |

Guides:

- `paper-explainer` — converting/summarizing a white paper, spec, or plan as a
  "scrollytelling" explainer: one figure (often a sequence diagram) pinned at
  the top of the view, whose elements are highlighted, annotated, added, or
  animated by a pure `setStep(n)` as the step text below scrolls past.

The MCP subprocess runs on the agent's host, so it reads and writes `path`
arguments itself and POSTs to `POST /internal/artifacts/{publish,list,revisions,read}`
with the agent's session ID. The daemon never touches agent-supplied paths.
Agents only see artifacts on their own host.

## Revisions and storage

- Every artifact is a line of revisions in a bare git repository,
  `$TANDEM_HOME/artifacts.git` (`internal/artifacts/repo.go`). Each artifact is
  the ref `refs/artifacts/<artifactId>` (`art-<16 hex>`); each revision is a
  commit on it whose tree holds the document as `index.html` and whose message
  is the title plus the revision metadata as JSON (`revisionNumber`, `title`,
  `filename`, `size`, `contentSha256`, publishing `sessionId`, `createdAt`,
  …). Only plumbing (`hash-object`, `mktree`, `commit-tree`, `update-ref` with
  the expected old value) is used, so writers share no work tree and an update
  is a compare-and-swap. Limit: 10 MB of UTF-8 per revision.
- Each publish appends an `artifact` event to the publishing session's log
  with the revision metadata, including `revision` (the commit). A card
  previews exactly that revision, so updating an artifact adds a card to the
  updating chat and leaves cards in other chats on the revisions they
  announced.
- Browsers fetch a revision from
  `GET /api/agents/<session>/artifacts/<artifactId>/<revision>` (bearer token).
  The session only routes the request: a federated session's artifacts live in
  its host's repository and are fetched over the tunnel
  (`get_artifact_revision`). Responses carry `Content-Security-Policy: sandbox`
  and `Content-Disposition: attachment`, and cache as immutable.
- Artifacts published before revisions existed have events without
  `revision` whose `artifactId` is the SHA-256 of the document, stored as a
  session asset (`GET /api/agents/<session>/assets/<artifactId>`). On boot the
  daemon imports each into the repository as a one-revision artifact under
  that same ID, so they appear in the gallery and agents can revise them; the
  old events are left alone and still fetch the asset.

## UI

- `ui/src/artifacts.ts` — route helpers, a small immutable-content cache, and
  the iframe sandbox flags.
- `ui/src/components/ArtifactView.tsx` — `ArtifactCard` (transcript preview: the
  page rendered at 1280×800 and CSS-scaled to the card, inert, lazily loaded
  when scrolled near) and `ArtifactPage` (full window).
- The full page is the hash route
  `#/artifact/<sessionId>/<artifactId>[/<revision>]`, pushed
  onto browser history so Back (button, Esc, or the browser's own) returns to
  the chat. A hash route keeps the relative-base UI bundle loadable on reload.
  The page overlays the still-mounted app, so the chat is exactly as left.
- Artifacts render via `srcdoc` in a sandboxed iframe without
  `allow-same-origin`: scripts run with an opaque origin and cannot reach the
  bearer token in localStorage, cookies, or the app DOM. The preview allows
  only scripts; the full page also allows forms, modals, popups and downloads.

## Gallery

"Artifacts Gallery" in the command palette (`artifacts.gallery`) opens
`ui/src/components/ArtifactsGallery.tsx`: the latest revision of every
artifact on every readable host (`list_artifacts`, see `docs/ws-protocol.md`),
newest first, as lazily loaded preview tiles with a fuzzy title/session filter
(Enter opens the top match). An artifact opens on the usual full page over the
gallery; Back returns to it.

## Revision picker

The full page's header has a revision picker (`list_artifact_revisions`)
labelled `rev N · time · session`. Choosing one replaces the route's revision
without a new history entry, so Back still returns to the chat or gallery the
page was opened from. A pre-revision link (no revision in the route) is
matched to its revision by `contentSha256`.
