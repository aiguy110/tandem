import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AppearanceModal } from './AppearanceModal';
import { useStore } from '../store';
import { DEFAULT_APPEARANCE } from '../appearance';

beforeEach(() => useStore.setState({ appearance: DEFAULT_APPEARANCE, theme: 'dark', modal: 'appearance' }));
afterEach(() => {
  cleanup();
  localStorage.clear();
});

// Two sliders in DOM order: app, then terminal. Matching them by accessible
// name would also match the neighbouring +/- buttons' aria-labels.
const slider = (which: 'app' | 'term') => screen.getAllByRole('slider')[which === 'app' ? 0 : 1];

const sizes = () => {
  const a = useStore.getState().appearance;
  return [a.uiFontSize, a.termFontSize];
};

describe('AppearanceModal', () => {
  it('drags the terminal size along while the sizes are locked', () => {
    render(<AppearanceModal />);
    fireEvent.change(slider('app'), { target: { value: '18' } });
    expect(sizes()).toEqual([18, 16]);
  });

  it('leaves the app size alone once unlocked', () => {
    render(<AppearanceModal />);
    fireEvent.click(screen.getByRole('button', { name: /Locked together/ }));
    fireEvent.change(slider('term'), { target: { value: '20' } });
    expect(sizes()).toEqual([13.5, 20]);
  });

  it('steps the size with the +/- buttons and persists the choice', () => {
    render(<AppearanceModal />);
    fireEvent.click(screen.getByRole('button', { name: 'Increase App' }));
    expect(useStore.getState().appearance.uiFontSize).toBe(14);
    expect(JSON.parse(localStorage.getItem('tandem.appearance')!).uiFontSize).toBe(14);
  });

  it('switches the theme', () => {
    render(<AppearanceModal />);
    fireEvent.click(screen.getByRole('radio', { name: /Light/ }));
    expect(useStore.getState().theme).toBe('light');
    expect(localStorage.getItem('tandem.theme')).toBe('light');
  });

  it('restores the defaults', () => {
    render(<AppearanceModal />);
    fireEvent.click(screen.getByRole('button', { name: 'Increase App' }));
    fireEvent.click(screen.getByRole('button', { name: 'Reset to defaults' }));
    expect(useStore.getState().appearance).toEqual(DEFAULT_APPEARANCE);
  });

  it('closes on Escape', () => {
    render(<AppearanceModal />);
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(useStore.getState().modal).toBe('none');
  });
});
