// Minimal markdown → HTML for streamed message chunks. `marked` in a locked-down
// config (no raw HTML passthrough) is enough for agent prose; this is a
// single-tenant localhost tool (D1/D15), not a public surface.

import { marked } from 'marked';
import katex from 'katex';

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

// Keep math as marked tokens instead of post-processing the generated HTML:
// that means a `$...$` inside a code span/fence stays literal code.  KaTeX is
// deliberately configured not to trust HTML-like commands in model output.
function renderMath(source: string, displayMode: boolean): string {
  return katex.renderToString(source.trim(), {
    displayMode,
    throwOnError: false,
    trust: false,
    strict: 'warn',
  });
}

marked.use({
  extensions: [
    {
      name: 'displayMath',
      level: 'block',
      start(src: string) {
        return src.search(/(?:^|\n)(?:\$\$|\\\[)/);
      },
      tokenizer(src: string) {
        const match = /^(?:\$\$\s*\n?([\s\S]*?)\n?\$\$|\\\[\s*\n?([\s\S]*?)\n?\\\])(?:\n|$)/.exec(src);
        if (!match) return undefined;
        return { type: 'displayMath', raw: match[0], text: match[1] ?? match[2] };
      },
      renderer(token: unknown) {
        return `<div class="math-display">${renderMath((token as { text: string }).text, true)}</div>\n`;
      },
    },
    {
      name: 'inlineMath',
      level: 'inline',
      start(src: string) {
        return src.search(/(?:\\\(|\$)/);
      },
      tokenizer(src: string) {
        const paren = /^\\\((.+?)\\\)/s.exec(src);
        if (paren) return { type: 'inlineMath', raw: paren[0], text: paren[1] };

        // Do not consume `$$` (the block tokenizer owns that syntax), and
        // require non-whitespace content to avoid treating currency as math.
        const dollar = /^\$(?!\$)([^\n$]*?\S[^\n$]*?)\$(?!\$)/.exec(src);
        if (dollar) return { type: 'inlineMath', raw: dollar[0], text: dollar[1] };
        return undefined;
      },
      renderer(token: unknown) {
        return renderMath((token as { text: string }).text, false);
      },
    },
  ],
});

export function renderMarkdown(src: string): string {
  // marked.parse can return a Promise only when async:true — we keep it sync.
  return marked.parse(src, { async: false }) as string;
}
