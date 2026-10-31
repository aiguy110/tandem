package artifactsmcp

// Docs is the artifact_docs tool's answer: agent-facing guidance for
// publishing artifacts. Keep it in step with docs/artifacts.md.
const Docs = `# Tandem artifacts

An artifact is a rendered document you publish for the user with artifact_publish.
The only type today is free-form HTML.

## What the user sees

- In the chat: a card with the title and a live, scaled-down preview of the page
  (rendered at a 1280px-wide desktop viewport, shrunk to fit the card, and
  non-interactive). Clicking it opens the artifact.
- Full page: the artifact fills the window with a Back button and a Download
  button that saves it as an .html file.
- Artifacts are part of the session transcript, so they survive reloads and
  reconnects and are visible from every device.

## Publishing

- Pass exactly one of:
  - path: an HTML file (absolute, or relative to your workspace). Best for large
    or iteratively edited documents: write the file, then publish it.
  - html: the complete document as a string. Fine for small one-offs.
- title: short and specific ("Q3 latency report", not "Report").
- filename: optional download name; .html is appended when missing.
- Each call creates a new artifact. To revise, edit and publish again; don't
  try to update an old one. Identical content yields the same artifactId.
- Limit: 10 MB of UTF-8 HTML.
- After publishing, tell the user briefly what it is. Don't paste the HTML into
  your reply; the card already shows it.

## Rendering environment

- The page runs in a sandboxed iframe with an opaque origin: scripts run, but
  the page cannot read Tandem's cookies, storage, or DOM, and same-origin
  APIs (localStorage, sessionStorage, IndexedDB, cookies) fail or are
  unavailable. Keep state in memory.
- Forms, modals (alert/confirm), and links that open new tabs work. Links
  without target="_blank" navigate the iframe itself; prefer target="_blank"
  for external links.
- There is no access to the agent's filesystem: relative URLs (./style.css,
  img/plot.png) will NOT resolve. Make the document self-contained:
  - inline CSS in <style> and JS in <script>;
  - embed images as data: URIs or inline <svg>;
  - CDN scripts/fonts (https://...) generally load, but the user may be offline
    or behind a proxy, so inline anything essential and degrade gracefully.
- The download is the same single file, so a self-contained document also
  makes the download work anywhere.

## Making it look good

- Always start with <!doctype html>, <meta charset="utf-8">, and
  <meta name="viewport" content="width=device-width, initial-scale=1">.
- Set your own background and text colours on html/body; the iframe does not
  inherit Tandem's theme. A neutral light page or a deliberate dark page both
  work; support prefers-color-scheme if you like.
- Design for widths from ~360px (phone) to wide desktop: use max-width
  containers, flex/grid, and responsive units. The chat preview shows the top
  of a 1280x800 viewport, so put a clear title and the key content first.
- Prefer system font stacks (system-ui, -apple-system, Segoe UI, sans-serif).
- For charts, inline SVG or a small inline script is most robust; if you use a
  charting library from a CDN, render a readable fallback if it fails to load.
- Avoid auto-playing audio, infinite heavy animations, and huge DOMs (the
  preview runs your scripts too).

## Good uses

Reports and summaries with tables or charts, visual diffs or comparisons,
interactive explorers or calculators, diagrams, mockups and UI prototypes,
rendered documentation.
`
