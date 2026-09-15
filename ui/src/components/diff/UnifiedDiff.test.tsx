import { fireEvent, render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { UnifiedDiff } from './UnifiedDiff';

describe('UnifiedDiff', () => {
  it('renders a non-empty patch without repeatedly resetting its open state', () => {
    const view = render(<UnifiedDiff patch={'diff --git a/file.txt b/file.txt\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-before\n+after'} />);

    expect(view.getByText('file.txt')).toBeTruthy();
    expect(view.container.querySelector('.diff-line.add .diff-text')?.textContent).toBe('+after');

    const file = view.container.querySelector('details')!;
    fireEvent(file, new Event('toggle'));
    expect(file.open).toBe(true);
  });
});
