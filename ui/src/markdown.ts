// Minimal markdown → HTML for streamed message chunks. `marked` in a locked-down
// config (no raw HTML passthrough) is enough for agent prose; this is a
// single-tenant localhost tool (D1/D15), not a public surface.

import { marked } from 'marked';

marked.setOptions({ gfm: true, breaks: true });

function escapeHtml(text: string): string {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// Wrap fenced code blocks with a copy button; TranscriptPane handles the
// click via event delegation since this HTML is injected via innerHTML.
const renderer = new marked.Renderer();
renderer.code = ({ text, lang }) => {
  const langString = (lang || '').match(/^\S*/)?.[0];
  const cls = langString ? ` class="language-${escapeHtml(langString)}"` : '';
  const code = escapeHtml(text.replace(/\n$/, '')) + '\n';
  return (
    `<div class="code-block">` +
    `<button type="button" class="code-copy-btn" data-copy-btn aria-label="Copy code">Copy</button>` +
    `<pre><code${cls}>${code}</code></pre>` +
    `</div>\n`
  );
};
marked.use({ renderer });

export function renderMarkdown(src: string): string {
  // marked.parse can return a Promise only when async:true — we keep it sync.
  return marked.parse(src, { async: false }) as string;
}
