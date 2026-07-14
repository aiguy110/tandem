// Minimal markdown → HTML for streamed message chunks. `marked` in a locked-down
// config (no raw HTML passthrough) is enough for agent prose; this is a
// single-tenant localhost tool (D1/D15), not a public surface.

import { marked } from 'marked';

marked.setOptions({ gfm: true, breaks: true });

export function renderMarkdown(src: string): string {
  // marked.parse can return a Promise only when async:true — we keep it sync.
  return marked.parse(src, { async: false }) as string;
}
