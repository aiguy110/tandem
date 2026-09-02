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

describe('renderMarkdown strikethrough', () => {
  it('strikes delimiters surrounded by whitespace outside and text inside', () => {
    expect(renderMarkdown('a ~~gone~~ b')).toContain('<del>gone</del>');
    expect(renderMarkdown('~~gone~~')).toContain('<del>gone</del>');
    expect(renderMarkdown('a ~gone~ b')).toContain('<del>gone</del>');
    expect(renderMarkdown('~~two words~~ after')).toContain('<del>two words</del>');
  });

  it('renders inline markup inside a strikethrough', () => {
    expect(renderMarkdown('~~a **b**~~')).toContain('<del>a <strong>b</strong></del>');
  });

  it('leaves approximation tildes alone', () => {
    for (const src of [
      'takes ~5 minutes',
      'about ~10 and ~20 items',
      '10~20 items',
      'a~b~c',
      'roughly ~ 5',
      'x ~~ y',
      'costs ~$5 and ~$9',
      'range ~5-~10 units',
    ]) {
      expect(renderMarkdown(src)).not.toContain('<del>');
    }
  });

  it('requires whitespace before the opener and after the closer', () => {
    expect(renderMarkdown('pre~~gone~~ post')).not.toContain('<del>');
    expect(renderMarkdown('pre ~~gone~~post')).not.toContain('<del>');
  });

  it('requires non-whitespace inside both delimiters', () => {
    expect(renderMarkdown('a ~~ gone~~ b')).not.toContain('<del>');
    expect(renderMarkdown('a ~~gone ~~ b')).not.toContain('<del>');
  });

  it('keeps tildes literal inside code', () => {
    const html = renderMarkdown('`~~code~~`\n\n```text\n~~fence~~\n```');
    expect(html).not.toContain('<del>');
    expect(html).toContain('~~code~~');
    expect(html).toContain('~~fence~~');
  });
});
