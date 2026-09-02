import { describe, expect, it } from 'vitest';
import { createUnifiedPatch } from './textDiff';

describe('createUnifiedPatch', () => {
  it('preserves context around a replacement', () => {
    expect(createUnifiedPatch('file.txt', 'one\ntwo\nthree\n', 'one\nchanged\nthree\n')).toContain(
      '@@ -1,3 +1,3 @@\n one\n-two\n+changed\n three',
    );
  });

  it('creates separate focused hunks for distant edits', () => {
    const before = Array.from({ length: 20 }, (_, index) => `line ${index + 1}`).join('\n');
    const after = before.replace('line 2', 'second').replace('line 19', 'nineteenth');
    const patch = createUnifiedPatch('file.txt', before, after);

    expect(patch.match(/^@@/gm)).toHaveLength(2);
    expect(patch).not.toContain(' line 10');
    expect(patch).toContain('-line 2\n+second');
    expect(patch).toContain('-line 19\n+nineteenth');
  });

  it('handles pure creation and deletion', () => {
    expect(createUnifiedPatch('new.txt', '', 'one\ntwo\n')).toContain('@@ -1,0 +1,2 @@\n+one\n+two');
    expect(createUnifiedPatch('old.txt', 'one\ntwo\n', '')).toContain('@@ -1,2 +1,0 @@\n-one\n-two');
  });
});
