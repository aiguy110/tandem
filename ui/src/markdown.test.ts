import { describe, expect, it } from 'vitest';
import { renderMarkdown } from './markdown';

describe('renderMarkdown math', () => {
  it('renders inline dollar and parenthesized LaTex', () => {
    const html = renderMarkdown('A $K_{7,1}$ matrix and \\(V^T\\).');
    expect(html).toContain('class="katex"');
    expect(html).toContain('K');
    expect(html).toContain('V');
  });

  it('renders bracketed and dollar-delimited display math', () => {
    const html = renderMarkdown('\\[\\nK_{7,1}^{\\text{translated}} = K_{7,1}R\\n\\]\n\n$$\\nV_{7,1}S\\n$$');
    expect(html.match(/class="math-display"/g)).toHaveLength(2);
    expect(html).toContain('katex-display');
  });

  it('does not interpret math delimiters inside code', () => {
    const html = renderMarkdown('`$not_math$`\n\n```text\n\\[not_math\\]\n```');
    expect(html).not.toContain('class="katex"');
    expect(html).toContain('$not_math$');
    expect(html).toContain('\\[not_math\\]');
  });
});
